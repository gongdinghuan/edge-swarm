package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGeneratePreservesRequestAndStreamsContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer backend-key" || body["model"] != "m" || body["max_tokens"] != float64(12) || body["stream"] != true {
			t.Errorf("wrong backend request: %s %+v", r.URL, body)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"世界\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	b, err := New(server.URL+"/v1", "backend-key")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	err = b.Generate(context.Background(), json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"test"}],"max_tokens":12}`), func(delta string) error { output.WriteString(delta); return nil })
	if err != nil || output.String() != "你好世界" {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
}
func TestMissingDoneIsFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer s.Close()
	b, _ := New(s.URL, "")
	if err := b.Generate(context.Background(), json.RawMessage(`{"model":"m"}`), func(string) error { return nil }); err == nil {
		t.Fatal("truncated backend stream reported success")
	}
}
func TestBackendErrorAndCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"denied"}`)
			return
		}
		<-r.Context().Done()
	}))
	defer s.Close()
	b, _ := New(s.URL, "bad")
	if err := b.Generate(context.Background(), json.RawMessage(`{}`), func(string) error { return nil }); err == nil {
		t.Fatal("401 swallowed")
	}
	b, _ = New(s.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := b.Generate(ctx, json.RawMessage(`{}`), func(string) error { return nil }); err == nil {
		t.Fatal("cancellation swallowed")
	}
}

func TestGeneratePreservesFinishReason(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer s.Close()
	b, _ := New(s.URL, "")
	reason, err := b.GenerateWithFinishReason(context.Background(), json.RawMessage(`{"model":"m"}`), func(string) error { return nil })
	if err != nil || reason != "length" {
		t.Fatalf("finish reason=%q err=%v", reason, err)
	}
}

func TestProbeRequiresExactModelAndUsesBackendKey(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer s.Close()
	b, _ := New(s.URL+"/v1/", "key")
	if err := b.Probe(context.Background(), "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.Probe(context.Background(), "model-b"); err == nil {
		t.Fatal("reported a missing model as ready")
	}
}

func TestNamedSSEErrorCannotBeFollowedBySuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\nevent: error\ndata: {\"message\":\"upstream failed\"}\n\ndata: [DONE]\n\n")
	}))
	defer s.Close()
	b, _ := New(s.URL, "")
	if err := b.Generate(context.Background(), json.RawMessage(`{"model":"m"}`), func(string) error { return nil }); err == nil {
		t.Fatal("backend SSE error reported success")
	}
}
