package ari

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fakeAsterisk(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if r.URL.Path == "/ari/events" {
			if r.URL.Query().Get("api_key") != "irnoti:s3cret" || r.URL.Query().Get("app") != "irnoti" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			for _, e := range events {
				_ = c.Write(r.Context(), websocket.MessageText, []byte(e))
			}
			time.Sleep(50 * time.Millisecond)
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if !ok || user != "irnoti" || pass != "s3cret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/ari/asterisk/info":
			_, _ = io.WriteString(w, `{"system":{"version":"22.1.0"}}`)
		case "/ari/channels":
			if r.Method == http.MethodPost {
				b, _ := io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"id":"c1","echo":`+string(b)+`}`)
				return
			}
			_, _ = io.WriteString(w, `[{"id":"a"},{"id":"b"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestClientAddsCredentialsAndPassesBodies(t *testing.T) {
	srv := fakeAsterisk(t, nil)
	defer srv.Close()
	c := New(srv.URL, "irnoti", "s3cret")
	ver, err := c.Info(context.Background())
	if err != nil || ver != "22.1.0" {
		t.Fatalf("info: %q %v", ver, err)
	}
	if n := c.ChannelCount(context.Background()); n != 2 {
		t.Errorf("channels = %d", n)
	}
	status, body, err := c.Do(context.Background(), "POST", "/channels?app=irnoti", []byte(`{"variables":{"A":"1"}}`))
	if err != nil || status != 200 || !strings.Contains(string(body), `"A":"1"`) {
		t.Errorf("do: %d %s %v", status, body, err)
	}
	if status, _, _ := c.Do(context.Background(), "GET", "/nothing", nil); status != 404 {
		t.Errorf("ARI statuses must pass through unchanged, got %d", status)
	}
}

func TestWrongCredentialsAreReported(t *testing.T) {
	srv := fakeAsterisk(t, nil)
	defer srv.Close()
	if _, err := New(srv.URL, "irnoti", "wrong").Info(context.Background()); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("got %v", err)
	}
}

func TestStreamDeliversEventsAndReconnects(t *testing.T) {
	srv := fakeAsterisk(t, []string{`{"type":"StasisStart","channel":{"id":"c1"}}`, `{"type":"ChannelDtmfReceived","digit":"5"}`})
	defer srv.Close()
	c := New(srv.URL, "irnoti", "s3cret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 10)
	up := make(chan bool, 10)
	go c.Stream(ctx, "irnoti", func(e json.RawMessage) { got <- string(e) }, func(b bool) { up <- b }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, want := range []string{"StasisStart", "ChannelDtmfReceived"} {
		select {
		case e := <-got:
			if !strings.Contains(e, want) {
				t.Errorf("event %s, want %s", e, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
		}
	}
	// the server closes after its events; the client must report down and come back up (second batch proves the reconnect)
	deadline := time.After(5 * time.Second)
	for seen := 0; seen < 2; {
		select {
		case e := <-got:
			if strings.Contains(e, "StasisStart") {
				seen++
			}
		case <-deadline:
			t.Fatal("did not reconnect")
		}
	}
}
