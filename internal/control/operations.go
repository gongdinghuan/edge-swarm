package control

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"edgeswarm/internal/store"
)

//go:embed console/*
var console embed.FS

func (s *Server) operations(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", consoleAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /assets/console.css", consoleAsset("console.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /assets/console.js", consoleAsset("console.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.db.Ping(ctx) != nil {
			problem(w, 503, "database_unavailable")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /swarm/v1/nodes", s.nodes)
	mux.HandleFunc("POST /swarm/v1/nodes/{id}/drain", s.drain(true))
	mux.HandleFunc("POST /swarm/v1/nodes/{id}/resume", s.drain(false))
	mux.HandleFunc("GET /swarm/v1/jobs", s.listTasks("batch"))
	mux.HandleFunc("GET /swarm/v1/requests", s.listTasks("chat"))
	mux.HandleFunc("GET /swarm/v1/batches/{id}", s.batchGroup)
	mux.HandleFunc("POST /swarm/v1/batches/{id}/cancel", s.cancelBatchGroup)
}

func consoleAsset(name, contentType string) http.HandlerFunc {
	data, err := console.ReadFile("console/" + name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Write(data)
	}
}

type nodeView struct {
	ID          string    `json:"id"`
	Model       string    `json:"model"`
	State       string    `json:"state"`
	Connected   bool      `json:"connected"`
	Ready       bool      `json:"ready"`
	GPU         string    `json:"gpu,omitempty"`
	VRAMMiB     int       `json:"vram_mib,omitempty"`
	BackendKind string    `json:"backend_kind,omitempty"`
	Active      bool      `json:"active"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	if !s.admin(w, r) {
		return
	}
	s.mu.Lock()
	registered, err := s.db.ListNodes()
	list := make([]nodeView, 0, len(registered))
	for _, n := range registered {
		view := nodeView{ID: n.ID, Model: n.Model, State: "offline"}
		if ss := s.sessions[n.ID]; ss != nil && !n.Revoked {
			view.Connected = true
			view.Ready = ss.ready
			view.GPU = ss.gpu
			view.VRAMMiB = ss.vramMiB
			view.BackendKind = ss.backendKind
			view.Active = ss.active != nil
			view.LastSeen = ss.lastSeen
			if view.Ready {
				view.State = "ready"
			} else {
				view.State = "probing"
			}
		}
		if n.Draining {
			view.State = "draining"
			view.Ready = false
		}
		if n.Revoked {
			view.State = "revoked"
			view.Ready = false
		}
		list = append(list, view)
	}
	s.mu.Unlock()
	if err != nil {
		problem(w, 500, "storage_error")
		return
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) drain(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.admin(w, r) {
			return
		}
		id := r.PathValue("id")
		// Serialize the persisted admission decision with dispatch. A task already
		// running is left alone; no new START can cross a completed drain request.
		s.mu.Lock()
		err := s.db.SetDraining(id, enabled)
		if err == nil {
			if ss := s.sessions[id]; ss != nil {
				ss.draining = enabled
			}
		}
		s.mu.Unlock()
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problem(w, 404, "not_found")
			} else {
				problem(w, 500, "storage_error")
			}
			return
		}
		state := "resumed"
		if enabled {
			state = "draining"
		}
		writeJSON(w, 200, map[string]string{"status": state})
	}
}

func (s *Server) listTasks(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := s.tenant(w, r)
		if !ok {
			return
		}
		limit := 25
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 100 {
				problem(w, 400, "invalid_limit")
				return
			}
			limit = n
		}
		items, err := s.db.ListTasks(tenant, kind, limit)
		if err != nil {
			problem(w, 500, "storage_error")
			return
		}
		if items == nil {
			items = []store.Task{}
		}
		writeJSON(w, 200, map[string]any{"data": items})
	}
}

func taskKind(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/swarm/v1/jobs/") {
		return "batch"
	}
	return "chat"
}

// The hint is scoped to tenant and model. This is observed request locality,
// not a claim about GPU KV residency or a portable KV-cache format.
func prefixHint(j store.Task) [32]byte {
	var req chatRequest
	if json.Unmarshal([]byte(j.Request), &req) != nil || len(req.Messages) == 0 {
		return [32]byte{}
	}
	first := req.Messages[0]
	// Prefer a shared system prompt or an exact first conversational turn.
	return sha256.Sum256([]byte(j.Tenant + "\x00" + j.Model + "\x00" + first.Role + "\x00" + first.Content))
}

func chooseNode(candidates []*session, j store.Task) *session {
	hint := prefixHint(j)
	now := time.Now()
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		ah := hint != [32]byte{} && a.warmHint == hint && now.Sub(a.warmAt) < 10*time.Minute
		bh := hint != [32]byte{} && b.warmHint == hint && now.Sub(b.warmAt) < 10*time.Minute
		if ah != bh {
			return ah
		}
		if !a.lastUsed.Equal(b.lastUsed) {
			return a.lastUsed.Before(b.lastUsed)
		}
		return a.node < b.node
	})
	return candidates[0]
}
