package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"edgeswarm/internal/store"
)

func (s *Server) batch(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		problem(w, 413, "batch_too_large")
		return
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		problem(w, 400, "invalid_request")
		return
	}
	if _, manifest := fields["items"]; !manifest {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		j, _, ok := s.create(w, r, "batch")
		if ok {
			writeJSON(w, 202, j)
		}
		return
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil || len(body.Items) == 0 || len(body.Items) > 20 {
		problem(w, 400, "invalid_batch_manifest")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 256 {
		problem(w, 400, "invalid_idempotency_key")
		return
	}
	deadline, err := requestDeadline(r)
	if err != nil {
		problem(w, 400, "invalid_deadline")
		return
	}
	items := make([]store.BatchInput, 0, len(body.Items))
	for _, item := range body.Items {
		child := r.Clone(r.Context())
		child.Body = io.NopCloser(bytes.NewReader(item))
		req, normalized, err := parse(w, child)
		if err != nil || req.Stream {
			problem(w, 400, "invalid_batch_item")
			return
		}
		if !s.allowedModel(tenant, req.Model) {
			problem(w, 404, "model_not_available")
			return
		}
		items = append(items, store.BatchInput{Model: req.Model, Request: normalized})
	}
	group, existing, err := s.db.CreateBatchGroup(tenant, key, items, deadline)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			problem(w, 409, "idempotency_conflict")
		case errors.Is(err, store.ErrCapacity):
			w.Header().Set("Retry-After", "5")
			problem(w, 429, "queue_full")
		default:
			problem(w, 500, "storage_error")
		}
		return
	}
	if existing {
		writeJSON(w, 200, group)
	} else {
		writeJSON(w, 202, group)
	}
}

func (s *Server) batchGroup(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	group, err := s.db.GetBatchGroup(tenant, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			problem(w, 404, "not_found")
		} else {
			problem(w, 500, "storage_error")
		}
		return
	}
	writeJSON(w, 200, group)
}

func (s *Server) cancelBatchGroup(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	err := s.db.CancelBatchGroup(tenant, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			problem(w, 404, "not_found")
		} else {
			problem(w, 500, "storage_error")
		}
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancel_requested"})
}
