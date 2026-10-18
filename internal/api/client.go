// Package api is the agent's side of the HTTPS protocol described in docs/PROTOCOL.md.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/irnoti/asterisk-agent/internal/version"
)

// ErrUnauthorized means the token was revoked or is wrong; retrying will not help until an operator fixes it.
var ErrUnauthorized = errors.New("the API rejected the agent token (401)")

type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
	// Poll uses its own client: the server holds the request open for up to PollWait.
	Long *http.Client
}

func New(base, token string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/") + "/api/v1/agent", Token: token,
		HTTP: &http.Client{Timeout: 30 * time.Second},
		Long: &http.Client{Timeout: 60 * time.Second},
	}
}

type Hello struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Asterisk string `json:"asterisk"`
}

type HelloResp struct {
	AgentID       string `json:"agentId"`
	App           string `json:"app"`
	PollWaitSec   int    `json:"pollWaitSec"`
	ConfigVersion string `json:"configVersion"`
}

type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Sha256  string `json:"sha256"`
}

// Bundle is the dialplan the API wants on this PBX. The agent validates every name and decides itself where each file goes.
type Bundle struct {
	Version string `json:"version"`
	Files   []File `json:"files"`
}

type Request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
}

type Response struct {
	ID       string `json:"id"`
	Status   int    `json:"status"`
	Body     string `json:"body"`
	Encoding string `json:"encoding"` // utf8 | base64
	Error    string `json:"error,omitempty"`
}

type Heartbeat struct {
	Channels int  `json:"channels"`
	AriUp    bool `json:"ariUp"`
	Uptime   int  `json:"uptimeSec"`
}

type HeartbeatResp struct {
	ConfigVersion string `json:"configVersion"`
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "irnoti-asterisk/"+version.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return res.StatusCode, ErrUnauthorized
	case res.StatusCode >= 300:
		return res.StatusCode, fmt.Errorf("%s %s: HTTP %d %s", method, path, res.StatusCode, truncate(string(data), 200))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s %s: bad JSON: %w", method, path, err)
		}
	}
	return res.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (c *Client) Hello(ctx context.Context, h Hello) (HelloResp, error) {
	var out HelloResp
	_, err := c.do(ctx, c.HTTP, http.MethodPost, "/hello", h, &out)
	return out, err
}

func (c *Client) Config(ctx context.Context) (Bundle, error) {
	var out Bundle
	_, err := c.do(ctx, c.HTTP, http.MethodGet, "/config", nil, &out)
	return out, err
}

// Poll waits (server side) for requests the API wants forwarded to ARI; an empty slice means "nothing within the wait, ask again".
func (c *Client) Poll(ctx context.Context) ([]Request, error) {
	var out struct {
		Requests []Request `json:"requests"`
	}
	_, err := c.do(ctx, c.Long, http.MethodPost, "/poll", struct{}{}, &out)
	return out.Requests, err
}

func (c *Client) Respond(ctx context.Context, r Response) error {
	_, err := c.do(ctx, c.HTTP, http.MethodPost, "/respond", r, nil)
	return err
}

func (c *Client) Events(ctx context.Context, events []json.RawMessage) error {
	_, err := c.do(ctx, c.HTTP, http.MethodPost, "/events", map[string]any{"events": events}, nil)
	return err
}

func (c *Client) Heartbeat(ctx context.Context, h Heartbeat) (HeartbeatResp, error) {
	var out HeartbeatResp
	_, err := c.do(ctx, c.HTTP, http.MethodPost, "/heartbeat", h, &out)
	return out, err
}
