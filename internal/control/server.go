package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"edgeswarm/internal/protocol"
	"edgeswarm/internal/store"
	"github.com/coder/websocket"
)

type Config struct {
	APIKeys     map[string]string
	AdminToken  string
	ModelAccess map[string][]string
}
type session struct {
	node, model      string
	epoch            int64
	conn             *websocket.Conn
	send             chan protocol.Message
	active           *store.Task
	seq              int64
	lastSeen         time.Time
	ready, draining  bool
	gpu, backendKind string
	vramMiB          int
	lastUsed, warmAt time.Time
	warmHint         [32]byte
}
type Server struct {
	db        *store.Store
	cfg       Config
	mu        sync.Mutex
	sessions  map[string]*session
	listeners map[string]chan string
}

func New(db *store.Store, cfg Config) *Server {
	return &Server{db: db, cfg: cfg, sessions: map[string]*session{}, listeners: map[string]chan string{}}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.operations(mux)
	mux.HandleFunc("POST /swarm/v1/nodes/enroll", s.enroll)
	mux.HandleFunc("POST /swarm/v1/nodes/{id}/revoke", s.revoke)
	mux.HandleFunc("GET /swarm/v1/agent/ws", s.connect)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /swarm/v1/batches", s.batch)
	mux.HandleFunc("GET /v1/models", s.models)
	for _, path := range []string{"requests", "jobs"} {
		mux.HandleFunc("GET /swarm/v1/"+path+"/{id}", s.query)
		mux.HandleFunc("POST /swarm/v1/"+path+"/{id}/cancel", s.cancel)
	}
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}
func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if len(value) < 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return ""
	}
	return value[7:]
}
func equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (s *Server) tenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := bearer(r)
	for candidate, t := range s.cfg.APIKeys {
		if equal(key, candidate) {
			return t, true
		}
	}
	problem(w, 401, "unauthorized")
	return "", false
}
func (s *Server) admin(w http.ResponseWriter, r *http.Request) bool {
	if equal(bearer(r), s.cfg.AdminToken) {
		return true
	}
	problem(w, 403, "admin_required")
	return false
}
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Model == "" || len(body.Model) > 200 {
		problem(w, 400, "invalid_model")
		return
	}
	id, token := store.ID(), store.ID()+store.ID()
	if err := s.db.AddNode(id, token, body.Model); err != nil {
		problem(w, 500, "storage_error")
		return
	}
	writeJSON(w, 201, map[string]string{"node_id": id, "token": token, "model": body.Model})
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.db.Revoke(id); err != nil {
		problem(w, 404, "not_found")
		return
	}
	s.mu.Lock()
	if ss := s.sessions[id]; ss != nil {
		ss.conn.CloseNow()
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	j, err := s.db.Get(tenant, r.PathValue("id"))
	if err != nil || j.Kind != taskKind(r) {
		problem(w, 404, "not_found")
		return
	}
	writeJSON(w, 200, j)
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	j, err := s.db.Get(tenant, r.PathValue("id"))
	if err != nil || j.Kind != taskKind(r) {
		problem(w, 404, "not_found")
		return
	}
	if err := s.db.Cancel(tenant, r.PathValue("id")); err != nil {
		problem(w, 404, "not_found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancel_requested"})
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	seen := map[string]bool{}
	data := []map[string]string{}
	for _, ss := range s.sessions {
		if ss.ready && !ss.draining && !seen[ss.model] && s.allowedModel(tenant, ss.model) {
			seen[ss.model] = true
			data = append(data, map[string]string{"id": ss.model, "object": "model"})
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) allowedModel(tenant, model string) bool {
	if s.cfg.ModelAccess == nil {
		return true
	}
	for _, allowed := range s.cfg.ModelAccess[tenant] {
		if model == allowed {
			return true
		}
	}
	return false
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
}

func parse(w http.ResponseWriter, r *http.Request) (chatRequest, string, error) {
	var body chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return body, "", errors.New("trailing JSON")
	}
	if body.Model == "" || len(body.Model) > 200 || len(body.Messages) == 0 || len(body.Messages) > 128 || body.MaxTokens < 0 || body.MaxTokens > 4096 {
		return body, "", errors.New("invalid request bounds")
	}
	if body.MaxTokens == 0 {
		body.MaxTokens = 512
	}
	for _, m := range body.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return body, "", errors.New("unsupported role")
		}
		if m.Content == "" {
			return body, "", errors.New("empty content")
		}
	}
	if body.Temperature != nil && (*body.Temperature < 0 || *body.Temperature > 2) {
		return body, "", errors.New("invalid temperature")
	}
	if body.TopP != nil && (*body.TopP <= 0 || *body.TopP > 1) {
		return body, "", errors.New("invalid top_p")
	}
	if len(body.Stop) > 0 {
		var one string
		var many []string
		if json.Unmarshal(body.Stop, &one) != nil && json.Unmarshal(body.Stop, &many) != nil {
			return body, "", errors.New("invalid stop")
		}
	}
	encoded, err := json.Marshal(body)
	return body, string(encoded), err
}
func (s *Server) create(w http.ResponseWriter, r *http.Request, kind string) (store.Task, chan string, bool) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return store.Task{}, nil, false
	}
	body, raw, err := parse(w, r)
	if err != nil {
		problem(w, 400, "invalid_request")
		return store.Task{}, nil, false
	}
	if !s.allowedModel(tenant, body.Model) {
		problem(w, 404, "model_not_available")
		return store.Task{}, nil, false
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 256 || strings.HasPrefix(key, "@batch:") {
		problem(w, 400, "invalid_idempotency_key")
		return store.Task{}, nil, false
	}
	deadline, err := requestDeadline(r)
	if err != nil {
		problem(w, 400, "invalid_deadline")
		return store.Task{}, nil, false
	}
	s.mu.Lock()
	j, exists, err := s.db.Create(tenant, key, kind, body.Model, raw, deadline)
	var ch chan string
	if err == nil && !exists && kind == "chat" && body.Stream {
		ch = make(chan string, 64)
		s.listeners[j.ID] = ch
	}
	s.mu.Unlock()
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrConflict) {
			status = 409
		}
		if errors.Is(err, store.ErrCapacity) {
			status = 429
			w.Header().Set("Retry-After", "5")
		}
		problem(w, status, "create_failed")
		return store.Task{}, nil, false
	}
	w.Header().Set("X-Request-ID", j.ID)
	if exists {
		writeJSON(w, 409, map[string]any{"error": map[string]string{"code": "request_exists"}, "request_id": j.ID, "status": j.Status})
		return j, nil, false
	}
	return j, ch, true
}
func requestDeadline(r *http.Request) (time.Time, error) {
	deadline := 120 * time.Second
	if raw := r.Header.Get("X-Swarm-Deadline-Ms"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 100 || ms > 600000 {
			return time.Time{}, errors.New("invalid deadline")
		}
		deadline = time.Duration(ms) * time.Millisecond
	}
	return time.Now().Add(deadline), nil
}
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	j, ch, ok := s.create(w, r, "chat")
	if !ok {
		return
	}
	defer func() { s.mu.Lock(); delete(s.listeners, j.ID); s.mu.Unlock() }()
	ctx, cancel := context.WithDeadline(r.Context(), time.UnixMilli(j.Deadline))
	defer cancel()
	controller := http.NewResponseController(w)
	controller.SetWriteDeadline(time.UnixMilli(j.Deadline))
	writeFrame := func(frame string) error {
		limit := time.Now().Add(5 * time.Second)
		if taskLimit := time.UnixMilli(j.Deadline); taskLimit.Before(limit) {
			limit = taskLimit
		}
		if err := controller.SetWriteDeadline(limit); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := io.WriteString(w, frame); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		controller.SetWriteDeadline(time.UnixMilli(j.Deadline))
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	started := false
	send := func(delta string) error {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)
			started = true
		}
		encoded, _ := json.Marshal(map[string]any{"id": j.ID, "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": delta}}}})
		return writeFrame(fmt.Sprintf("data: %s\n\n", encoded))
	}
	for {
		select {
		case <-ctx.Done():
			s.db.Cancel(j.Tenant, j.ID)
			if started {
				writeFrame("data: {\"error\":{\"code\":\"interrupted\"}}\n\n")
			} else {
				problem(w, 504, "deadline_or_client_cancelled")
			}
			return
		case delta := <-ch:
			if err := send(delta); err != nil {
				s.db.Cancel(j.Tenant, j.ID)
				return
			}
		case <-ticker.C:
			current, err := s.db.Get(j.Tenant, j.ID)
			if err != nil {
				if started {
					writeFrame("data: {\"error\":{\"code\":\"storage_error\"}}\n\n")
				} else {
					problem(w, 500, "storage_error")
				}
				return
			}
			if current.Status == "queued" || current.Status == "running" {
				continue
			}
			if current.Status == "succeeded" {
				if ch != nil {
					for {
						select {
						case delta := <-ch:
							if err := send(delta); err != nil {
								return
							}
						default:
							goto drained
						}
					}
				drained:
					if !started {
						if err := send(""); err != nil {
							return
						}
					}
					terminal, _ := json.Marshal(map[string]any{"id": j.ID, "model": j.Model, "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason(current.FinishReason)}}})
					if writeFrame(fmt.Sprintf("data: %s\n\n", terminal)) == nil {
						writeFrame("data: [DONE]\n\n")
					}
				} else {
					writeJSON(w, 200, map[string]any{"id": j.ID, "object": "chat.completion", "model": j.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": current.Result}, "finish_reason": finishReason(current.FinishReason)}}})
				}
				return
			}
			if started {
				writeFrame("data: {\"error\":{\"code\":\"execution_interrupted\"}}\n\n")
			} else {
				problem(w, 502, "execution_failed")
			}
			return
		}
	}
}

func finishReason(reason string) string {
	switch reason {
	case "stop", "length", "content_filter":
		return reason
	default:
		return "stop"
	}
}

func send(ss *session, m protocol.Message) bool {
	m.Version = protocol.Version
	select {
	case ss.send <- m:
		return true
	default:
		ss.conn.CloseNow()
		return false
	}
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		problem(w, 403, "browser_not_allowed")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		problem(w, 426, "websocket_upgrade_required")
		return
	}
	id := r.URL.Query().Get("node_id")
	epoch, err := s.db.Connect(id, bearer(r))
	if err != nil {
		if errors.Is(err, store.ErrUnauthorized) {
			problem(w, 401, "unauthorized_node")
		} else {
			problem(w, 503, "storage_unavailable")
		}
		return
	}
	model, err := s.db.NodeModel(id, epoch)
	if err != nil {
		// The credential was just accepted. A newer connection may have won
		// the epoch race; allow retry rather than treating it as revocation.
		problem(w, 503, "connection_not_current")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1024*1024 + 8192)
	ss := &session{node: id, model: model, epoch: epoch, conn: conn, send: make(chan protocol.Message, 128), lastSeen: time.Now()}
	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		cancel()
		conn.CloseNow()
		s.mu.Lock()
		current := s.sessions[id] == ss
		if current {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
		if current {
			// Reap can recover fenced work on its next pass. If the database
			// is unavailable, the task's existing lease still bounds recovery.
			_ = s.db.Disconnect(id, epoch)
		}
	}()
	s.mu.Lock()
	// A delayed upgrade must not displace a newer epoch for this identity.
	if _, err := s.db.NodeModel(id, epoch); err != nil {
		s.mu.Unlock()
		return
	}
	registered, err := s.db.ListNodes()
	if err != nil {
		s.mu.Unlock()
		return
	}
	for _, n := range registered {
		if n.ID == id {
			ss.draining = n.Draining
			break
		}
	}
	if old := s.sessions[id]; old != nil {
		if old.epoch >= epoch {
			s.mu.Unlock()
			return
		}
		old.conn.CloseNow()
	}
	s.sessions[id] = ss
	send(ss, protocol.Message{Type: "hello", Epoch: epoch})
	s.mu.Unlock()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-ss.send:
				data, e := json.Marshal(m)
				if e != nil {
					return
				}
				writeCtx, end := context.WithTimeout(ctx, 5*time.Second)
				e = conn.Write(writeCtx, websocket.MessageText, data)
				end()
				if e != nil {
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m protocol.Message
		if json.Unmarshal(data, &m) != nil || m.Version != protocol.Version || m.Epoch != epoch {
			return
		}
		s.mu.Lock()
		if s.sessions[id] != ss {
			s.mu.Unlock()
			return
		}
		ss.lastSeen = time.Now()
		s.handleMessage(ss, m)
		s.mu.Unlock()
	}
}
func (s *Server) handleMessage(ss *session, m protocol.Message) {
	if m.Type == "heartbeat" {
		if _, err := s.db.NodeModel(ss.node, ss.epoch); err != nil {
			ss.conn.CloseNow()
			return
		}
		ss.ready = m.Ready == nil || *m.Ready
		if m.GPU != "" && len(m.GPU) <= 200 {
			ss.gpu = m.GPU
		}
		if m.VRAMMiB > 0 && m.VRAMMiB <= 4*1024*1024 {
			ss.vramMiB = m.VRAMMiB
		}
		if m.BackendKind != "" && len(m.BackendKind) <= 40 {
			ss.backendKind = m.BackendKind
		}
		return
	}
	j := ss.active
	if j == nil || j.ID != m.ID || j.Fence != m.Fence {
		return
	}
	now := time.Now()
	switch m.Type {
	case "renew":
		if s.db.Renew(j.ID, ss.node, ss.epoch, j.Fence, now, 30*time.Second) == nil {
			send(ss, protocol.Message{Type: "lease", ID: j.ID, Fence: j.Fence, Epoch: ss.epoch, LeaseUntil: min(now.Add(30*time.Second).UnixMilli(), j.Deadline)})
		} else {
			send(ss, protocol.Message{Type: "cancel", ID: j.ID, Fence: j.Fence, Epoch: ss.epoch})
		}
	case "delta":
		if m.Seq != ss.seq+1 {
			send(ss, protocol.Message{Type: "cancel", ID: j.ID, Fence: j.Fence, Epoch: ss.epoch})
			s.db.Fail(j.ID, ss.node, ss.epoch, j.Fence, "invalid output sequence", now)
			return
		}
		if ch := s.listeners[j.ID]; ch != nil {
			if s.db.MarkOutput(j.ID, ss.node, ss.epoch, j.Fence, now) != nil {
				// Poison this connection before returning. A later result must
				// never turn a discarded token into apparent stream success.
				ss.conn.CloseNow()
				s.db.Fail(j.ID, ss.node, ss.epoch, j.Fence, "output authority could not be persisted", now)
				return
			}
			select {
			case ch <- m.Content:
			default:
				if s.db.Cancel(j.Tenant, j.ID) != nil {
					ss.conn.CloseNow()
				}
			}
		}
		ss.seq = m.Seq
	case "result":
		if s.db.CompleteWithReason(j.ID, ss.node, ss.epoch, j.Fence, m.Content, finishReason(m.FinishReason), now) == nil {
			ss.warmHint = prefixHint(*j)
			ss.warmAt = now
		}
		ss.active = nil
	case "failed":
		s.db.Fail(j.ID, ss.node, ss.epoch, j.Fence, "backend execution failed", now)
		ss.active = nil
	}
}
func (s *Server) Run(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer func() {
		s.mu.Lock()
		for _, ss := range s.sessions {
			ss.conn.CloseNow()
		}
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if s.db.Reap(now) != nil {
				continue
			}
			s.mu.Lock()
			idle := map[string][]*session{}
			for _, ss := range s.sessions {
				if now.Sub(ss.lastSeen) > 15*time.Second {
					ss.conn.CloseNow()
					continue
				}
				if ss.active != nil {
					j, err := s.db.Lookup(ss.active.ID)
					if err == nil && j.Status != "running" {
						send(ss, protocol.Message{Type: "cancel", ID: j.ID, Fence: ss.active.Fence, Epoch: ss.epoch})
					}
					continue
				}
				if ss.ready && !ss.draining {
					idle[ss.model] = append(idle[ss.model], ss)
				}
			}
			for model, candidates := range idle {
				for len(candidates) > 0 {
					now = time.Now()
					next, err := s.db.NextQueued(model, now)
					if err != nil {
						break
					}
					ss := chooseNode(candidates, next)
					candidates = candidates[1:]
					j, err := s.db.Claim(ss.node, ss.epoch, model, now, 30*time.Second)
					if err != nil {
						continue
					}
					ss.active = &j
					ss.seq = 0
					ss.lastUsed = now
					send(ss, protocol.Message{Type: "start", ID: j.ID, Fence: j.Fence, Epoch: ss.epoch, Deadline: j.Deadline, LeaseUntil: j.Lease, Request: json.RawMessage(j.Request)})
				}
			}
			s.mu.Unlock()
		}
	}
}
