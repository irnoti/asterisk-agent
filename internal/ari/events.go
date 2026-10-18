package ari

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Stream keeps the ARI event WebSocket open for the Stasis app and hands every event to `emit`. It reconnects with backoff until ctx
// ends; `state` is told when the stream goes up or down so the heartbeat can report it.
func (c *Client) Stream(ctx context.Context, app string, emit func(json.RawMessage), state func(up bool), log *slog.Logger) {
	u, err := url.Parse(c.Base)
	if err != nil {
		log.Error("bad ARI url", "err", err)
		return
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ari/events"
	q := url.Values{"app": {app}, "api_key": {c.User + ":" + c.Password}}
	u.RawQuery = q.Encode()

	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.streamOnce(ctx, u.String(), emit, state)
		state(false)
		if ctx.Err() != nil {
			return
		}
		// a connection that lived a while was healthy: start the backoff over
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		log.Warn("ARI event stream closed", "err", err, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, wsURL string, emit func(json.RawMessage), state func(bool)) error {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)
	state(true)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		emit(json.RawMessage(data))
	}
}
