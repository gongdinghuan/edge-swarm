package control_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeswarm/internal/agent"
	"edgeswarm/internal/control"
	"edgeswarm/internal/store"
)

func TestPublicNetworkVerticalSlice(t *testing.T) {
	testPublicNetworkVerticalSlice(t, "sqlite", filepath.Join(t.TempDir(), "test.db"), false)
}

func TestTLSNetworkVerticalSlice(t *testing.T) {
	testPublicNetworkVerticalSlice(t, "sqlite", filepath.Join(t.TempDir(), "tls.db"), true)
}

func TestPostgresNetworkVerticalSlice(t *testing.T) {
	dsn := os.Getenv("SWARM_TEST_E2E_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("dedicated PostgreSQL end-to-end database not configured")
	}
	testPublicNetworkVerticalSlice(t, "pgx", dsn, true)
}

func testPublicNetworkVerticalSlice(t *testing.T, driver, dsn string, tls bool) {
	t.Helper()
	db, err := store.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"client": "tenant", "other": "other"}, AdminToken: "admin"})
	go srv.Run(ctx)
	httpServer := httptest.NewUnstartedServer(srv.Handler())
	if tls {
		httpServer.StartTLS()
	} else {
		httpServer.Start()
	}
	defer httpServer.Close()
	call := func(method, path, key, body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		res, e := httpServer.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	denied := call("POST", "/swarm/v1/nodes/enroll", "client", `{"model":"m"}`)
	denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatalf("enrollment allowed: %d", denied.StatusCode)
	}
	enrolled := call("POST", "/swarm/v1/nodes/enroll", "admin", `{"model":"m"}`)
	var identity struct {
		NodeID string `json:"node_id"`
		Token  string `json:"token"`
	}
	json.NewDecoder(enrolled.Body).Decode(&identity)
	enrolled.Body.Close()
	if identity.Token == "" {
		t.Fatal("missing node identity")
	}
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello edge\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer modelServer.Close()
	a := agent.New(agent.Config{URL: httpServer.URL, NodeID: identity.NodeID, Token: identity.Token, BackendURL: modelServer.URL, Model: "m", ControlHTTPClient: httpServer.Client()})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	response := call("POST", "/v1/chat/completions", "client", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":12}`)
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(payload, []byte("hello edge")) || !bytes.Contains(payload, []byte("[DONE]")) {
		t.Fatalf("chat failed %d: %s", response.StatusCode, payload)
	}
	id := response.Header.Get("X-Request-ID")
	if id == "" {
		t.Fatal("missing request id")
	}
	foreign := call("GET", "/swarm/v1/requests/"+id, "other", "")
	foreign.Body.Close()
	if foreign.StatusCode != 404 {
		t.Fatal("cross tenant task exposed")
	}
	batch := call("POST", "/swarm/v1/batches", "client", `{"model":"m","messages":[{"role":"user","content":"batch"}],"max_tokens":12}`)
	var j store.Task
	json.NewDecoder(batch.Body).Decode(&j)
	batch.Body.Close()
	if batch.StatusCode != 202 {
		t.Fatalf("batch=%d", batch.StatusCode)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		j, err = db.Get("tenant", j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == "succeeded" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j.Status != "succeeded" || j.Result != "hello edge" {
		t.Fatalf("batch did not complete: %+v", j)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent failed to shut down")
	}
}

type stalledWriter struct {
	header   http.Header
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	deadline time.Time
}

func (w *stalledWriter) Header() http.Header                       { return w.header }
func (w *stalledWriter) WriteHeader(int)                           {}
func (w *stalledWriter) SetWriteDeadline(deadline time.Time) error { w.deadline = deadline; return nil }
func (w *stalledWriter) FlushError() error                         { return nil }
func (w *stalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	var expiry <-chan time.Time
	if !w.deadline.IsZero() {
		timer := time.NewTimer(time.Until(w.deadline))
		defer timer.Stop()
		expiry = timer.C
	}
	select {
	case <-w.release:
		return 0, io.ErrClosedPipe
	case <-expiry:
		return 0, context.DeadlineExceeded
	}
}

func TestSlowModelsClientDoesNotHoldGlobalLock(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "lock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}})
	handler := srv.Handler()
	stalled := &stalledWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	defer close(stalled.release)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer key")
	go handler.ServeHTTP(stalled, req)
	<-stalled.started
	completed := make(chan struct{})
	go func() {
		r := httptest.NewRequest("POST", "/swarm/v1/batches", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer key")
		handler.ServeHTTP(httptest.NewRecorder(), r)
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(300 * time.Millisecond):
		t.Error("slow models response holds global scheduler lock")
	}
}

func TestOutputPersistenceFailureCannotBecomeSuccessfulStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fault.db")
	db, err := store.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	faultDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	_, err = faultDB.Exec(`CREATE TABLE observed(value INTEGER); CREATE TRIGGER reject_output BEFORE UPDATE OF output_started ON tasks BEGIN INSERT INTO observed VALUES(1); SELECT RAISE(FAIL,'injected output failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}})
	go srv.Run(ctx)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	db.AddNode("node", "secret", "m")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		http.NewResponseController(w).Flush()
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			var count int
			faultDB.QueryRow(`SELECT COUNT(*) FROM observed`).Scan(&count)
			if count > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		faultDB.Exec(`DROP TRIGGER reject_output`)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	go agent.New(agent.Config{URL: front.URL, NodeID: "node", Token: "secret", Model: "m", BackendURL: model.URL}).Run(ctx)
	request, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("X-Swarm-Deadline-Ms", "4000")
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	if bytes.Contains(payload, []byte("[DONE]")) {
		t.Fatalf("discarded delta hidden by successful completion: %s", payload)
	}
	var observed int
	if err = faultDB.QueryRow(`SELECT COUNT(*) FROM observed`).Scan(&observed); err != nil || observed == 0 {
		t.Fatalf("fault did not execute: %d %v", observed, err)
	}
}

func TestSlowStreamWriteStopsAtRequestDeadline(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "slow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}})
	go srv.Run(ctx)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	db.AddNode("node", "secret", "m")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"text\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	go agent.New(agent.Config{URL: front.URL, NodeID: "node", Token: "secret", Model: "m", BackendURL: model.URL}).Run(ctx)
	stalled := &stalledWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	defer close(stalled.release)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer key")
	r.Header.Set("X-Swarm-Deadline-Ms", "500")
	done := make(chan struct{})
	go func() { srv.Handler().ServeHTTP(stalled, r); close(done) }()
	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("no stream content")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("blocked SSE write survived request deadline")
	}
}

func TestRequestValidationAndCancellation(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}, AdminToken: "admin"})
	h := srv.Handler()
	for _, body := range []string{`{"model":"m","messages":[],"max_tokens":1}`, `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[]}`, `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":-1}`} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid accepted %s: %d", body, w.Code)
		}
	}
	j, _, _ := db.Create("t", "id", "batch", "m", `{}`, time.Now().Add(time.Minute))
	r := httptest.NewRequest("POST", "/swarm/v1/jobs/"+j.ID+"/cancel", nil)
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel=%d %s", w.Code, w.Body)
	}
	got, _ := db.Get("t", j.ID)
	if got.Status != "cancelled" {
		t.Fatal("cancel not durable")
	}
}

func TestInterruptedStreamDoesNotPretendSuccess(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "interrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}})
	go srv.Run(ctx)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	db.AddNode("node", "secret", "m")
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		http.NewResponseController(w).Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer model.Close()
	worker := agent.New(agent.Config{URL: front.URL, NodeID: "node", Token: "secret", Model: "m", BackendURL: model.URL})
	go worker.Run(ctx)
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer key")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("partial")) || !bytes.Contains(payload, []byte("execution_interrupted")) || bytes.Contains(payload, []byte("[DONE]")) {
		t.Fatalf("wrong failure semantics: %s", payload)
	}
}

func TestRunningCancellationReachesBackend(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := control.New(db, control.Config{APIKeys: map[string]string{"key": "t"}})
	go srv.Run(ctx)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	db.AddNode("node", "secret", "m")
	cancelled := make(chan struct{}, 1)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	defer model.Close()
	worker := agent.New(agent.Config{URL: front.URL, NodeID: "node", Token: "secret", Model: "m", BackendURL: model.URL})
	go worker.Run(ctx)
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer key")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	stop, _ := http.NewRequest("POST", front.URL+"/swarm/v1/requests/"+res.Header.Get("X-Request-ID")+"/cancel", nil)
	stop.Header.Set("Authorization", "Bearer key")
	stopped, err := http.DefaultClient.Do(stop)
	if err != nil {
		t.Fatal(err)
	}
	stopped.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation never reached model server")
	}
}
