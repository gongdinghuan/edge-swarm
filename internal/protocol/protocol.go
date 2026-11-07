package protocol

import "encoding/json"

const Version = 1

// Message is never an execution credential by itself. The control service also
// checks the authenticated connection epoch, SQL lease and task fence.
type Message struct {
	Version      int             `json:"version"`
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	Epoch        int64           `json:"epoch,omitempty"`
	Fence        int64           `json:"fence,omitempty"`
	Seq          int64           `json:"seq,omitempty"`
	Deadline     int64           `json:"deadline,omitempty"`
	LeaseUntil   int64           `json:"lease_until,omitempty"`
	Request      json.RawMessage `json:"request,omitempty"`
	Content      string          `json:"content,omitempty"`
	Error        string          `json:"error,omitempty"`
	Ready        *bool           `json:"ready,omitempty"`
	GPU          string          `json:"gpu,omitempty"`
	VRAMMiB      int             `json:"vram_mib,omitempty"`
	BackendKind  string          `json:"backend_kind,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}
