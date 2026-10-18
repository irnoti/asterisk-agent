// Package tunnel is the daemon: it forwards the API's ARI requests to the local Asterisk and the local ARI events to the API.
// Every connection is outbound HTTPS, so the PBX opens no port and works behind NAT.
package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/irnoti/asterisk-agent/internal/api"
	"github.com/irnoti/asterisk-agent/internal/ari"
	"github.com/irnoti/asterisk-agent/internal/proxy"
	"github.com/irnoti/asterisk-agent/internal/version"
)

const (
	maxConcurrent  = 32
	eventQueueSize = 5000
	eventBatch     = 50
	heartbeatEvery = 30 * time.Second
)

// ARI is what the daemon needs from the local Asterisk (the real client, or a fake in tests).
type ARI interface {
	Do(ctx context.Context, method, target string, body []byte) (int, []byte, error)
	Info(ctx context.Context) (string, error)
	ChannelCount(ctx context.Context) int
	Stream(ctx context.Context, app string, emit func(json.RawMessage), state func(up bool), log *slog.Logger)
}

// API is the protocol client (see internal/api).
type API interface {
	Hello(ctx context.Context, h api.Hello) (api.HelloResp, error)
	Config(ctx context.Context) (api.Bundle, error)
	Poll(ctx context.Context) ([]api.Request, error)
	Respond(ctx context.Context, r api.Response) error
	Events(ctx context.Context, events []json.RawMessage) error
	Heartbeat(ctx context.Context, h api.Heartbeat) (api.HeartbeatResp, error)
}

type Agent struct {
	API  API
	ARI  ARI
	App  string
	Log  *slog.Logger
	Sync func(ctx context.Context, b api.Bundle) (bool, error)
	// Retry is the wait between failed attempts; tests shorten it.
	Retry func(attempt int) time.Duration

	started time.Time
	ariUp   atomic.Bool
	version atomic.Value // config version last applied
	events  chan json.RawMessage
	dropped atomic.Int64
}

func (a *Agent) retry(attempt int) time.Duration {
	if a.Retry != nil {
		return a.Retry(attempt)
	}
	d := time.Second << min(attempt, 5)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Run blocks until ctx ends. It never gives up: a PBX that loses the network keeps trying until it is back.
func (a *Agent) Run(ctx context.Context) error {
	a.started = time.Now()
	a.events = make(chan json.RawMessage, eventQueueSize)
	a.version.Store("")

	if err := a.hello(ctx); err != nil {
		return err
	}
	a.syncConfig(ctx)

	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { a.ARI.Stream(ctx, a.App, a.emit, func(up bool) { a.ariUp.Store(up) }, a.Log) })
	run(func() { a.sendEvents(ctx) })
	run(func() { a.heartbeats(ctx) })
	run(func() { a.pollLoop(ctx) })
	wg.Wait()
	return ctx.Err()
}

func (a *Agent) hello(ctx context.Context) error {
	host, _ := os.Hostname()
	for attempt := 0; ; attempt++ {
		ver, _ := a.ARI.Info(ctx)
		resp, err := a.API.Hello(ctx, api.Hello{Version: version.Version, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Asterisk: ver})
		if err == nil {
			a.Log.Info("connected to the API", "agent", resp.AgentID, "asterisk", ver)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := a.retry(attempt)
		if errors.Is(err, api.ErrUnauthorized) {
			a.Log.Error("the token was refused; waiting for an operator to fix it", "err", err)
			wait = max(wait, time.Minute)
			if a.Retry != nil {
				wait = a.Retry(attempt)
			}
		} else {
			a.Log.Warn("cannot reach the API", "err", err, "retryIn", wait.String())
		}
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
}

func (a *Agent) emit(ev json.RawMessage) {
	select {
	case a.events <- ev:
	default:
		// the API is unreachable and the queue is full: keep the newest events, a stale DTMF or hang-up is worth nothing later
		select {
		case <-a.events:
			a.dropped.Add(1)
		default:
		}
		select {
		case a.events <- ev:
		default:
		}
	}
}

func (a *Agent) sendEvents(ctx context.Context) {
	for {
		var batch []json.RawMessage
		select {
		case <-ctx.Done():
			return
		case ev := <-a.events:
			batch = append(batch, ev)
		}
		timer := time.After(20 * time.Millisecond)
	fill:
		for len(batch) < eventBatch {
			select {
			case ev := <-a.events:
				batch = append(batch, ev)
			case <-timer:
				break fill
			}
		}
		for attempt := 0; ctx.Err() == nil; attempt++ {
			err := a.API.Events(ctx, batch)
			if err == nil {
				break
			}
			if attempt >= 8 {
				a.Log.Error("dropping events the API would not take", "count", len(batch), "err", err)
				break
			}
			if !sleep(ctx, a.retry(attempt)) {
				return
			}
		}
	}
}

func (a *Agent) heartbeats(ctx context.Context) {
	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		a.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) beat(ctx context.Context) {
	resp, err := a.API.Heartbeat(ctx, api.Heartbeat{Channels: a.ARI.ChannelCount(ctx), AriUp: a.ariUp.Load(), Uptime: int(time.Since(a.started).Seconds())})
	if err != nil {
		a.Log.Warn("heartbeat failed", "err", err)
		return
	}
	if resp.ConfigVersion != "" && resp.ConfigVersion != a.version.Load().(string) {
		a.syncConfig(ctx)
	}
}

func (a *Agent) syncConfig(ctx context.Context) {
	if a.Sync == nil {
		return
	}
	b, err := a.API.Config(ctx)
	if err != nil {
		a.Log.Warn("cannot fetch the configuration", "err", err)
		return
	}
	changed, err := a.Sync(ctx, b)
	if err != nil {
		a.Log.Error("configuration not applied", "version", b.Version, "err", err)
		return
	}
	a.version.Store(b.Version)
	if changed {
		a.Log.Info("configuration updated", "version", b.Version)
	}
}

func (a *Agent) pollLoop(ctx context.Context) {
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()
	for attempt := 0; ctx.Err() == nil; {
		reqs, err := a.API.Poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := a.retry(attempt)
			if errors.Is(err, api.ErrUnauthorized) {
				wait = max(wait, 10*time.Second)
			}
			a.Log.Warn("poll failed", "err", err, "retryIn", wait.String())
			attempt++
			if !sleep(ctx, wait) {
				return
			}
			continue
		}
		attempt = 0
		for _, r := range reqs {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				a.handle(ctx, r)
			}()
		}
	}
}

func (a *Agent) handle(ctx context.Context, r api.Request) {
	res := api.Response{ID: r.ID, Encoding: "utf8"}
	if !proxy.Allowed(r.Method, r.Path) {
		a.Log.Warn("refused a request outside the allow-list", "method", r.Method, "path", r.Path)
		res.Status, res.Error = http.StatusForbidden, "not allowed"
	} else {
		cctx, cancel := context.WithTimeout(ctx, 65*time.Second)
		status, body, err := a.ARI.Do(cctx, r.Method, r.Path, []byte(r.Body))
		cancel()
		if err != nil {
			res.Status, res.Error = http.StatusBadGateway, err.Error()
		} else {
			res.Status = status
			// recordings are binary; everything else ARI says is JSON text
			if strings.HasSuffix(strings.SplitN(r.Path, "?", 2)[0], "/file") || !utf8.Valid(body) {
				res.Body, res.Encoding = base64.StdEncoding.EncodeToString(body), "base64"
			} else {
				res.Body = string(body)
			}
		}
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := a.API.Respond(ctx, res); err == nil {
			return
		} else if ctx.Err() != nil {
			return
		} else {
			a.Log.Warn("could not deliver a response", "id", r.ID, "err", err)
		}
		if !sleep(ctx, a.retry(attempt)) {
			return
		}
	}
}

var _ ARI = (*ari.Client)(nil)
var _ API = (*api.Client)(nil)
