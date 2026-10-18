package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsTokenAndParsesAnswers(t *testing.T) {
	var gotAuth, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotPath = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		switch r.URL.Path {
		case "/api/v1/agent/hello":
			_ = json.NewEncoder(w).Encode(HelloResp{AgentID: "9", PollWaitSec: 25})
		case "/api/v1/agent/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": []Request{{ID: "a", Method: "GET", Path: "/channels"}}})
		case "/api/v1/agent/config":
			_ = json.NewEncoder(w).Encode(Bundle{Version: "v3", Files: []File{{Name: "x.conf"}}})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "irna_secret")

	h, err := c.Hello(context.Background(), Hello{Version: "1"})
	if err != nil || h.AgentID != "9" || gotAuth != "Bearer irna_secret" || !strings.HasPrefix(gotUA, "irnoti-asterisk/") {
		t.Fatalf("hello: %+v %v auth=%q ua=%q", h, err, gotAuth, gotUA)
	}
	reqs, err := c.Poll(context.Background())
	if err != nil || len(reqs) != 1 || reqs[0].ID != "a" {
		t.Fatalf("poll: %+v %v", reqs, err)
	}
	b, err := c.Config(context.Background())
	if err != nil || b.Version != "v3" || gotPath != "/api/v1/agent/config" {
		t.Fatalf("config: %+v %v %s", b, err, gotPath)
	}
	if err := c.Respond(context.Background(), Response{ID: "a", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := c.Events(context.Background(), []json.RawMessage{json.RawMessage(`{"type":"x"}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/hello" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	if _, err := c.Hello(context.Background(), Hello{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401 must be ErrUnauthorized, got %v", err)
	}
	if _, err := c.Poll(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: %v", err)
	}
}
