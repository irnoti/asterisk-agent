package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/irnoti/asterisk-agent/internal/api"
)

type fakeAPI struct {
	mu        sync.Mutex
	helloErrs []error
	polls     chan []api.Request
	responses chan api.Response
	events    chan []json.RawMessage
	hbVersion string
	cfgCalls  int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{polls: make(chan []api.Request, 10), responses: make(chan api.Response, 10), events: make(chan []json.RawMessage, 10)}
}

func (f *fakeAPI) Hello(context.Context, api.Hello) (api.HelloResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.helloErrs) > 0 {
		e := f.helloErrs[0]
		f.helloErrs = f.helloErrs[1:]
		return api.HelloResp{}, e
	}
	return api.HelloResp{AgentID: "7"}, nil
}
func (f *fakeAPI) Config(context.Context) (api.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfgCalls++
	return api.Bundle{Version: "v1"}, nil
}
func (f *fakeAPI) Poll(ctx context.Context) ([]api.Request, error) {
	select {
	case r := <-f.polls:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return nil, nil
	}
}
func (f *fakeAPI) Respond(_ context.Context, r api.Response) error { f.responses <- r; return nil }
func (f *fakeAPI) Events(_ context.Context, e []json.RawMessage) error {
	f.events <- e
	return nil
}
func (f *fakeAPI) Heartbeat(context.Context, api.Heartbeat) (api.HeartbeatResp, error) {
	return api.HeartbeatResp{ConfigVersion: f.hbVersion}, nil
}

type fakeARI struct {
	mu      sync.Mutex
	calls   []string
	status  int
	body    []byte
	emitFn  func(json.RawMessage)
	started chan struct{}
}

func (f *fakeARI) Do(_ context.Context, method, target string, _ []byte) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+target)
	return f.status, f.body, nil
}
func (f *fakeARI) Info(context.Context) (string, error) { return "22.0.0", nil }
func (f *fakeARI) ChannelCount(context.Context) int     { return 3 }
func (f *fakeARI) Stream(ctx context.Context, _ string, emit func(json.RawMessage), state func(bool), _ *slog.Logger) {
	f.emitFn = emit
	state(true)
	close(f.started)
	<-ctx.Done()
}

func start(t *testing.T, fa *fakeAPI, fr *fakeARI) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fr.started = make(chan struct{})
	a := &Agent{API: fa, ARI: fr, App: "irnoti", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Retry: func(int) time.Duration { return time.Millisecond },
		Sync: func(context.Context, api.Bundle) (bool, error) { return true, nil }}
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	<-fr.started
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func recv[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

func TestForwardsAllowedRequestsAndRefusesTheRest(t *testing.T) {
	fa, fr := newFakeAPI(), &fakeARI{status: 200, body: []byte(`{"id":"c1"}`)}
	start(t, fa, fr)
	fa.polls <- []api.Request{
		{ID: "1", Method: "POST", Path: "/channels?endpoint=PJSIP/1001&app=irnoti"},
		{ID: "2", Method: "GET", Path: "/applications"},
		{ID: "3", Method: "POST", Path: "/asterisk/modules/res_pjsip.so"},
	}
	got := map[string]api.Response{}
	for i := 0; i < 3; i++ {
		r := recv(t, fa.responses)
		got[r.ID] = r
	}
	if got["1"].Status != 200 || got["1"].Body != `{"id":"c1"}` || got["1"].Encoding != "utf8" {
		t.Errorf("allowed request: %+v", got["1"])
	}
	if got["2"].Status != 403 || got["3"].Status != 403 {
		t.Errorf("refused requests: %+v %+v", got["2"], got["3"])
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.calls) != 1 || fr.calls[0] != "POST /channels?endpoint=PJSIP/1001&app=irnoti" {
		t.Errorf("ARI was asked: %v", fr.calls)
	}
}

func TestRecordingsComeBackAsBase64(t *testing.T) {
	audio := []byte{0x52, 0x49, 0x46, 0x46, 0xff, 0xfe, 0x00}
	fa, fr := newFakeAPI(), &fakeARI{status: 200, body: audio}
	start(t, fa, fr)
	fa.polls <- []api.Request{{ID: "r", Method: "GET", Path: "/recordings/stored/vm-1/file"}}
	r := recv(t, fa.responses)
	raw, err := base64.StdEncoding.DecodeString(r.Body)
	if r.Encoding != "base64" || err != nil || string(raw) != string(audio) {
		t.Errorf("%+v %v", r, err)
	}
}

func TestEventsAreBatchedToTheAPI(t *testing.T) {
	fa, fr := newFakeAPI(), &fakeARI{status: 200}
	start(t, fa, fr)
	for _, e := range []string{`{"type":"StasisStart"}`, `{"type":"ChannelDtmfReceived"}`} {
		fr.emitFn(json.RawMessage(e))
	}
	seen := 0
	for seen < 2 {
		seen += len(recv(t, fa.events))
	}
	if seen != 2 {
		t.Errorf("events delivered = %d", seen)
	}
}

func TestNewConfigVersionInHeartbeatTriggersOneSync(t *testing.T) {
	fa, fr := newFakeAPI(), &fakeARI{status: 200}
	fa.hbVersion = "v2"
	start(t, fa, fr)
	deadline := time.After(2 * time.Second)
	for {
		fa.mu.Lock()
		n := fa.cfgCalls
		fa.mu.Unlock()
		if n >= 2 { // the startup fetch plus the one caused by the heartbeat
			return
		}
		select {
		case <-deadline:
			t.Fatalf("config fetched %d times", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestHelloRetriesUntilTheAPIAnswers(t *testing.T) {
	fa, fr := newFakeAPI(), &fakeARI{status: 200}
	fa.helloErrs = []error{errors.New("connection refused"), api.ErrUnauthorized}
	start(t, fa, fr) // start waits for the ARI stream, which only starts after a successful hello
}
