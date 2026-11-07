package control_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeswarm/internal/control"
	"edgeswarm/internal/protocol"
	"edgeswarm/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOperatorAndTenantViews(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.AddNode("edge-a", "private-node-key", "m"); err != nil {
		t.Fatal(err)
	}
	own, _, err := db.Create("a", "a", "batch", "m", `{"secret":"do not list me"}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := db.Create("b", "b", "batch", "m", `{}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := control.New(db, control.Config{APIKeys: map[string]string{"a-key": "a"}, AdminToken: "admin-key"}).Handler()
	call := func(method, path, key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		h.ServeHTTP(w, r)
		return w
	}
	if r := call("GET", "/", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "蜂群") || r.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("console inaccessible or unsafe: %d", r.Code)
	}
	if r := call("GET", "/readyz", ""); r.Code != 200 {
		t.Fatalf("database readiness: %d", r.Code)
	}
	if r := call("GET", "/swarm/v1/nodes", "a-key"); r.Code != 403 {
		t.Fatalf("tenant accessed inventory: %d", r.Code)
	}
	if r := call("GET", "/swarm/v1/nodes", "admin-key"); r.Code != 200 || !strings.Contains(r.Body.String(), "edge-a") || strings.Contains(r.Body.String(), "private-node-key") {
		t.Fatalf("unexpected inventory: %d %s", r.Code, r.Body)
	}
	if r := call("GET", "/swarm/v1/jobs", "a-key"); r.Code != 200 || !strings.Contains(r.Body.String(), own.ID) || strings.Contains(r.Body.String(), other.ID) || strings.Contains(r.Body.String(), "do not list me") {
		t.Fatalf("tenant list disclosure: %d %s", r.Code, r.Body)
	}
	if r := call("GET", "/swarm/v1/requests/"+own.ID, "a-key"); r.Code != 404 {
		t.Fatalf("job visible under chat path: %d", r.Code)
	}
	if r := call("POST", "/swarm/v1/nodes/edge-a/drain", "admin-key"); r.Code != 200 {
		t.Fatalf("drain: %d %s", r.Code, r.Body)
	}
	if r := call("GET", "/swarm/v1/nodes", "admin-key"); !strings.Contains(r.Body.String(), `"state":"draining"`) {
		t.Fatalf("draining state missing: %s", r.Body)
	}
	if r := call("POST", "/swarm/v1/nodes/edge-a/resume", "admin-key"); r.Code != 200 {
		t.Fatalf("resume: %d %s", r.Code, r.Body)
	}
}

func TestReadyNodeDrainAndFinishReason(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "route.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.AddNode("edge", "token", "m"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s := control.New(db, control.Config{APIKeys: map[string]string{"api": "a"}, AdminToken: "admin"})
	go s.Run(ctx)
	front := httptest.NewServer(s.Handler())
	defer front.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/swarm/v1/agent/ws?node_id=edge", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer token"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var hello protocol.Message
	if err = wsjson.Read(ctx, conn, &hello); err != nil {
		t.Fatal(err)
	}
	send := func(m protocol.Message) {
		t.Helper()
		m.Version = protocol.Version
		m.Epoch = hello.Epoch
		if err = wsjson.Write(ctx, conn, m); err != nil {
			t.Fatal(err)
		}
	}
	request := func(method, path, key, body string) *http.Response {
		t.Helper()
		r, e := http.NewRequestWithContext(ctx, method, front.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Authorization", "Bearer "+key)
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	setDrain := func(action string) {
		t.Helper()
		r := request("POST", "/swarm/v1/nodes/edge/"+action, "admin", "")
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", action, r.StatusCode)
		}
	}
	ready := false
	send(protocol.Message{Type: "heartbeat", Ready: &ready, BackendKind: "local", GPU: "test GPU", VRAMMiB: 8192})
	resp := request("POST", "/swarm/v1/batches", "api", `{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	var job store.Task
	err = json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("submit: %d %v", resp.StatusCode, err)
	}
	waitQueued := func() {
		t.Helper()
		time.Sleep(220 * time.Millisecond)
		j, e := db.Get("a", job.ID)
		if e != nil || j.Status != "queued" {
			t.Fatalf("unavailable node claimed work: %+v %v", j, e)
		}
	}
	waitQueued()
	setDrain("drain")
	ready = true
	send(protocol.Message{Type: "heartbeat", Ready: &ready})
	waitQueued()
	setDrain("resume")
	var task protocol.Message
	if err = wsjson.Read(ctx, conn, &task); err != nil {
		t.Fatal(err)
	}
	if task.Type != "start" || task.ID != job.ID {
		t.Fatalf("unexpected start: %+v", task)
	}
	send(protocol.Message{Type: "result", ID: task.ID, Fence: task.Fence, Content: "completed", FinishReason: "length"})
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		res := request("GET", "/swarm/v1/jobs/"+job.ID, "api", "")
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(b), `"status":"succeeded"`) {
			if !strings.Contains(string(b), `"finish_reason":"length"`) {
				t.Fatalf("lost finish reason: %s", b)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never completed")
}

func TestBatchManifestAPI(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := control.New(db, control.Config{APIKeys: map[string]string{"key": "tenant", "other": "other"}, ModelAccess: map[string][]string{"tenant": {"m"}}}).Handler()
	run := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", "manifest")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	good := `{"items":[{"model":"m","messages":[{"role":"user","content":"one"}]},{"model":"m","messages":[{"role":"user","content":"two"}]}]}`
	bad := strings.Replace(good, `"model":"m","messages":[{"role":"user","content":"two"}]`, `"model":"other","messages":[{"role":"user","content":"two"}]`, 1)
	if r := run("POST", "/swarm/v1/batches", "key", bad); r.Code != 404 {
		t.Fatalf("model ACL bypass: %d %s", r.Code, r.Body)
	}
	if rows, err := db.ListTasks("tenant", "batch", 20); err != nil || len(rows) != 0 {
		t.Fatalf("invalid group partially enqueued: %+v %v", rows, err)
	}
	first := run("POST", "/swarm/v1/batches", "key", good)
	var batch store.BatchGroup
	if first.Code != 202 || json.Unmarshal(first.Body.Bytes(), &batch) != nil || len(batch.Jobs) != 2 {
		t.Fatalf("batch submission: %d %s", first.Code, first.Body)
	}
	duplicate := run("POST", "/swarm/v1/batches", "key", good)
	var again store.BatchGroup
	if duplicate.Code != 200 || json.Unmarshal(duplicate.Body.Bytes(), &again) != nil || again.ID != batch.ID {
		t.Fatalf("duplicate batch: %d %s", duplicate.Code, duplicate.Body)
	}
	path := "/swarm/v1/batches/" + batch.ID
	if r := run("GET", path, "other", ""); r.Code != 404 {
		t.Fatal("foreign batch exposed")
	}
	if r := run("POST", path+"/cancel", "key", ""); r.Code != 200 {
		t.Fatalf("cancel batch: %d %s", r.Code, r.Body)
	}
	if r := run("GET", path, "key", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel state: %d %s", r.Code, r.Body)
	}
}

func TestAgentHandshakeDatabaseFailureIsRetryable(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	handler := control.New(db, control.Config{}).Handler()
	db.Close()
	r := httptest.NewRequest("GET", "/swarm/v1/agent/ws?node_id=edge", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Authorization", "Bearer node-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("temporary storage failure returned permanent agent rejection: %d %s", w.Code, w.Body)
	}
}

func TestBatchRecoveryAcrossWorkers(t *testing.T) {
	t.Run("disconnected worker stays offline", func(t *testing.T) {
		testBatchRecoveryAcrossWorkers(t, false)
	})
	t.Run("worker replaced by a newer connection", func(t *testing.T) {
		testBatchRecoveryAcrossWorkers(t, true)
	})
}

func testBatchRecoveryAcrossWorkers(t *testing.T, reconnect bool) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "handoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, node := range []string{"first", "second"} {
		if err = db.AddNode(node, "secret", "m"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	srv := control.New(db, control.Config{})
	go srv.Run(ctx)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	dial := func(node string, ready bool) (*websocket.Conn, int64) {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/swarm/v1/agent/ws?node_id="+node, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer secret"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		var m protocol.Message
		if err = wsjson.Read(ctx, conn, &m); err != nil {
			t.Fatal(err)
		}
		if err = wsjson.Write(ctx, conn, protocol.Message{Version: protocol.Version, Type: "heartbeat", Epoch: m.Epoch, Ready: &ready}); err != nil {
			t.Fatal(err)
		}
		return conn, m.Epoch
	}
	first, oldEpoch := dial("first", true)
	task, _, err := db.Create("tenant", "work", "batch", "m", `{"model":"m","messages":[{"role":"user","content":"work"}]}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var old protocol.Message
	if err = wsjson.Read(ctx, first, &old); err != nil || old.Type != "start" || old.ID != task.ID {
		t.Fatalf("initial assignment: %+v %v", old, err)
	}
	var replacementEpoch int64
	if reconnect {
		_, replacementEpoch = dial("first", false)
		if replacementEpoch <= oldEpoch {
			t.Fatal("node did not reconnect with a new epoch")
		}
	} else {
		first.CloseNow()
	}
	other, otherEpoch := dial("second", true)
	var retry protocol.Message
	if err = wsjson.Read(ctx, other, &retry); err != nil || retry.ID != task.ID || retry.Fence <= old.Fence {
		t.Fatalf("reassignment: %+v %v", retry, err)
	}
	if err = wsjson.Write(ctx, other, protocol.Message{Version: protocol.Version, Type: "result", Epoch: otherEpoch, ID: retry.ID, Fence: retry.Fence, Content: "recovered", FinishReason: "stop"}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		j, e := db.Get("tenant", task.ID)
		if e == nil && j.Status == "succeeded" {
			if j.Attempts != 2 || j.Node != "second" || j.Result != "recovered" {
				t.Fatalf("unexpected final task: %+v", j)
			}
			if reconnect {
				if _, err := db.NodeModel("first", replacementEpoch); err != nil {
					t.Fatalf("replacement connection lost authority: %v", err)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reassigned work was not completed")
}
