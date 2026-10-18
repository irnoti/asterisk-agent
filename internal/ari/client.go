// Package ari talks to the Asterisk REST Interface on this machine.
package ari

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxBody caps what is read back from ARI (a stored recording is the largest thing that legitimately passes).
const MaxBody = 25 << 20

// Client is a minimal ARI REST client with the agent's credentials.
type Client struct {
	Base     string // e.g. http://127.0.0.1:8088
	User     string
	Password string
	HTTP     *http.Client
}

func New(base, user, password string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), User: user, Password: password, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Do forwards one request. `target` is path plus query without the `/ari` prefix, exactly as the API sent it.
func (c *Client) Do(ctx context.Context, method, target string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/ari"+target, rd)
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(c.User, c.Password)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(data) > MaxBody {
		return 0, nil, fmt.Errorf("ARI answer larger than %d bytes", MaxBody)
	}
	return res.StatusCode, data, nil
}

// Info returns the Asterisk version, which doubles as a "can we reach ARI with our credentials" check.
func (c *Client) Info(ctx context.Context) (string, error) {
	status, data, err := c.Do(ctx, http.MethodGet, "/asterisk/info", nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", fmt.Errorf("ARI rejected the agent's credentials (401)")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("ARI answered %d", status)
	}
	var v struct {
		System struct {
			Version string `json:"version"`
		} `json:"system"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", err
	}
	return v.System.Version, nil
}

// ChannelCount is the number of live channels, for the heartbeat. -1 when it cannot be read.
func (c *Client) ChannelCount(ctx context.Context) int {
	status, data, err := c.Do(ctx, http.MethodGet, "/channels", nil)
	if err != nil || status != http.StatusOK {
		return -1
	}
	var list []json.RawMessage
	if json.Unmarshal(data, &list) != nil {
		return -1
	}
	return len(list)
}
