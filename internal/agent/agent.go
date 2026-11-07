// Package agent runs an outbound-only, one-slot worker. The backend endpoint is
// configured by the machine owner, never selected from an incoming task.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"edgeswarm/internal/backend"
	"edgeswarm/internal/protocol"
	"github.com/coder/websocket"
)

type Config struct {
	URL, NodeID, Token, Model, BackendURL, BackendKey string
	ProbeBackend                                      bool
	GPU, BackendKind                                  string
	VRAMMiB                                           int
	// ControlHTTPClient permits an application to supply its own trusted CA.
	ControlHTTPClient *http.Client
}
type Agent struct{ cfg Config }

func New(cfg Config) *Agent { return &Agent{cfg: cfg} }

func (a *Agent) endpoints() (*url.URL, *backend.OpenAI, error) {
	engine, err := backend.New(a.cfg.BackendURL, a.cfg.BackendKey)
	if err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(a.cfg.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, nil, errors.New("invalid control URL")
	}
	if a.cfg.NodeID == "" || a.cfg.Token == "" || a.cfg.Model == "" {
		return nil, nil, errors.New("node_id, token and model are required")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if host := u.Hostname(); host != "localhost" && !net.ParseIP(host).IsLoopback() {
			return nil, nil, errors.New("public control connections require HTTPS")
		}
	default:
		return nil, nil, errors.New("control URL must be http(s)")
	}
	return u, engine, nil
}

// Check diagnoses dependencies without opening a worker session.
func (a *Agent) Check(ctx context.Context) error {
	u, engine, err := a.endpoints()
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/readyz"
	u.RawPath, u.RawQuery, u.ForceQuery = "", "", false
	controlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(controlCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("invalid control readiness URL")
	}
	client := a.controlClient()
	res, err := client.Do(req)
	if err != nil {
		return errors.New("control readiness is unreachable; check DNS, outbound access and TLS trust")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("control readiness HTTP %d; check the center and HTTPS proxy", res.StatusCode)
	}
	var readiness struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&readiness); err != nil || readiness.Status != "ready" {
		return errors.New("unexpected control readiness response; check the center URL and proxy routing")
	}
	probeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	return engine.Probe(probeCtx, a.cfg.Model)
}

func (a *Agent) Run(ctx context.Context) error {
	u, engine, err := a.endpoints()
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/swarm/v1/agent/ws"
	q := u.Query()
	q.Set("node_id", a.cfg.NodeID)
	u.RawQuery = q.Encode()
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.connect(ctx, u.String(), engine)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errControlCredentials) {
			return err
		}
		if time.Since(started) >= 30*time.Second {
			backoff = time.Second
		}
		delay := backoff/2 + rand.N(backoff/2)
		log.Printf("agent connection ended: %v; retrying in %s", err, delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil
}

type execution struct {
	m          protocol.Message
	cancel     context.CancelFunc
	leaseUntil time.Time
}

var errControlCredentials = errors.New("control rejected node credentials")

func (a *Agent) controlClient() http.Client {
	client := http.Client{}
	if a.cfg.ControlHTTPClient != nil {
		client = *a.cfg.ControlHTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func (a *Agent) connect(parent context.Context, address string, engine *backend.OpenAI) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+a.cfg.Token)
	dialCtx, end := context.WithTimeout(ctx, 10*time.Second)
	client := a.controlClient()
	conn, res, err := websocket.Dial(dialCtx, address, &websocket.DialOptions{HTTPHeader: headers, HTTPClient: &client})
	end()
	if err != nil {
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		if res != nil {
			if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
				return fmt.Errorf("%w (HTTP %d)", errControlCredentials, res.StatusCode)
			}
			return fmt.Errorf("control handshake HTTP %d", res.StatusCode)
		}
		return errors.New("control is unreachable or TLS verification failed")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(128 * 1024)
	helloCtx, finishHello := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := conn.Read(helloCtx)
	finishHello()
	if err != nil {
		return errors.New("control hello was not received")
	}
	var hello protocol.Message
	if json.Unmarshal(data, &hello) != nil || hello.Type != "hello" || hello.Version != protocol.Version {
		return errors.New("invalid control hello or protocol version")
	}
	epoch := hello.Epoch
	output := make(chan protocol.Message, 128)
	var mu sync.Mutex
	var active *execution
	var workers sync.WaitGroup
	emit := func(m protocol.Message) error {
		m.Version = protocol.Version
		m.Epoch = epoch
		select {
		case output <- m:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			cancel()
			return errors.New("transport backpressure")
		}
	}
	defer func() {
		cancel()
		mu.Lock()
		if active != nil {
			active.cancel()
		}
		mu.Unlock()
		workers.Wait()
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-output:
				raw, _ := json.Marshal(m)
				writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, raw)
				stop()
				if err != nil {
					return
				}
			}
		}
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		var lastProbeError string
		heartbeat := func() {
			msg := protocol.Message{Type: "heartbeat", GPU: a.cfg.GPU, VRAMMiB: a.cfg.VRAMMiB, BackendKind: a.cfg.BackendKind}
			if a.cfg.ProbeBackend {
				probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
				err := engine.Probe(probeCtx, a.cfg.Model)
				stop()
				ready := err == nil
				msg.Ready = &ready
				if err != nil && err.Error() != lastProbeError {
					lastProbeError = err.Error()
					log.Printf("agent backend unavailable: %v", err)
				} else if ready && lastProbeError != "" {
					log.Print("agent backend ready")
					lastProbeError = ""
				}
			}
			emit(msg)
		}
		heartbeat()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		count := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				count++
				mu.Lock()
				current := active
				if current != nil && time.Now().After(current.leaseUntil) {
					current.cancel()
				}
				mu.Unlock()
				if current != nil && count%10 == 0 {
					emit(protocol.Message{Type: "renew", ID: current.m.ID, Fence: current.m.Fence})
				}
				if count%5 == 0 {
					heartbeat()
				}
			}
		}
	}()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return errors.New("control WebSocket closed")
		}
		var m protocol.Message
		if json.Unmarshal(raw, &m) != nil || m.Version != protocol.Version || m.Epoch != epoch {
			return errors.New("invalid control message or connection epoch")
		}
		mu.Lock()
		switch m.Type {
		case "start":
			if active != nil {
				mu.Unlock()
				emit(protocol.Message{Type: "failed", ID: m.ID, Fence: m.Fence, Error: "capacity occupied"})
				continue
			}
			var req struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(m.Request, &req) != nil || req.Model != a.cfg.Model {
				mu.Unlock()
				emit(protocol.Message{Type: "failed", ID: m.ID, Fence: m.Fence, Error: "model mismatch"})
				continue
			}
			jobCtx, jobCancel := context.WithDeadline(ctx, time.UnixMilli(m.Deadline))
			current := &execution{m: m, cancel: jobCancel, leaseUntil: time.Now().Add(25 * time.Second)}
			active = current
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer jobCancel()
				var content strings.Builder
				var seq int64
				reason, err := engine.GenerateWithFinishReason(jobCtx, m.Request, func(delta string) error {
					if content.Len()+len(delta) > 1024*1024 {
						return errors.New("output limit exceeded")
					}
					content.WriteString(delta)
					seq++
					return emit(protocol.Message{Type: "delta", ID: m.ID, Fence: m.Fence, Seq: seq, Content: delta})
				})
				result := protocol.Message{Type: "result", ID: m.ID, Fence: m.Fence, Content: content.String(), FinishReason: reason}
				if err != nil {
					result.Type = "failed"
					result.Content = ""
					result.Error = "backend execution failed"
				}
				// Clear locally before result delivery; control will dispatch the next task
				// only after observing the result, so no two generations execute together.
				mu.Lock()
				if active == current {
					active = nil
				}
				mu.Unlock()
				emit(result)
			}()
		case "cancel":
			if active != nil && active.m.ID == m.ID && active.m.Fence == m.Fence {
				active.cancel()
			}
		case "lease":
			if active != nil && active.m.ID == m.ID && active.m.Fence == m.Fence {
				active.leaseUntil = time.Now().Add(25 * time.Second)
			}
		default:
			mu.Unlock()
			return errors.New("unsupported control message")
		}
		mu.Unlock()
	}
}
