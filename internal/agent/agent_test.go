package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"edgeswarm/internal/protocol"
	"github.com/coder/websocket"
)

func TestCheckUsesReadinessAndExactBackendModelWithoutSendingNodeToken(t *testing.T) {
	control := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/readyz" || r.Header.Get("Authorization") != "" {
			t.Error("readiness request used an unexpected route or sent credentials")
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"status":"ready"}`)
	}))
	defer control.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer local-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer model.Close()
	cfg := Config{URL: control.URL + "/prefix/", NodeID: "node", Token: "node-secret", Model: "m",
		BackendURL: model.URL + "/v1", BackendKey: "local-key", ControlHTTPClient: control.Client()}
	if err := New(cfg).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Model = "missing"
	if err := New(cfg).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "absent from backend model list") {
		t.Fatalf("expected an exact model mismatch, got %v", err)
	}
	cfg.Model = "m"
	cfg.BackendKey = "wrong-key"
	if err := New(cfg).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("expected a safe backend authorization error, got %v", err)
	}
	cfg.BackendKey = "local-key"
	cfg.ControlHTTPClient = nil
	if err := New(cfg).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("expected rejection of an untrusted certificate, got %v", err)
	}
}

func TestControlRejectsRevokedCredentialsWithoutReconnect(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			control := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer control.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a := New(Config{URL: control.URL, NodeID: "node", Token: "secret", Model: "m", BackendURL: "http://127.0.0.1", ControlHTTPClient: control.Client()})
			err := a.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("expected a safe terminal status %d error, got %v", status, err)
			}
		})
	}
}

func TestAgentReconnectsAfterControlUnavailable(t *testing.T) {
	var attempts atomic.Int32
	connected := make(chan struct{}, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"version":1,"type":"hello","epoch":2}`)); err != nil {
			return
		}
		if _, _, err := conn.Read(r.Context()); err == nil {
			connected <- struct{}{}
		}
	}))
	defer control.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a := New(Config{URL: control.URL, NodeID: "node", Token: "token", Model: "m", BackendURL: "http://127.0.0.1"})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("agent did not recover from a temporary control failure")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFirstHeartbeatReportsBackendReadyAndGPU(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"configured"}]}`)
	}))
	defer model.Close()

	messages := make(chan protocol.Message, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"version":1,"type":"hello","epoch":42}`)); err != nil {
			return
		}
		_, raw, err := conn.Read(ctx)
		if err == nil {
			var msg protocol.Message
			if json.Unmarshal(raw, &msg) == nil {
				messages <- msg
			}
		}
	}))
	defer control.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := New(Config{URL: control.URL, NodeID: "node", Token: "token", Model: "configured", BackendURL: model.URL + "/v1", ProbeBackend: true, GPU: "test GPU", VRAMMiB: 8192, BackendKind: "llama.cpp"})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case msg := <-messages:
		if msg.Type != "heartbeat" || msg.Epoch != 42 || msg.Ready == nil || !*msg.Ready || msg.GPU != "test GPU" || msg.VRAMMiB != 8192 || msg.BackendKind != "llama.cpp" {
			t.Fatalf("unexpected first heartbeat: %+v", msg)
		}
	case <-ctx.Done():
		t.Fatal("agent did not report ready immediately after hello")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
