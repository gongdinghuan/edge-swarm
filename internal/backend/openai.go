package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OpenAI struct {
	base, key string
	client    *http.Client
}

func New(base, key string) (*OpenAI, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("backend URL must be http(s) without credentials, query, or fragment")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &OpenAI{base: strings.TrimRight(base, "/"), key: key, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("backend redirects disabled") }}}, nil
}

func (b *OpenAI) Probe(ctx context.Context, model string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"/models", nil)
	if err != nil {
		return err
	}
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	res, err := b.client.Do(req)
	if err != nil {
		return errors.New("backend model list is unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("backend model list HTTP %d", res.StatusCode)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1024*1024)).Decode(&response); err != nil {
		return errors.New("invalid backend model list")
	}
	for _, entry := range response.Data {
		if entry.ID == model {
			return nil
		}
	}
	return errors.New("configured model is absent from backend model list")
}

func (b *OpenAI) Generate(ctx context.Context, raw json.RawMessage, emit func(string) error) error {
	_, err := b.GenerateWithFinishReason(ctx, raw, emit)
	return err
}

func (b *OpenAI) GenerateWithFinishReason(ctx context.Context, raw json.RawMessage, emit func(string) error) (string, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	if body == nil {
		return "", errors.New("backend request must be a JSON object")
	}
	body["stream"] = true
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	res, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("backend HTTP %d", res.StatusCode)
	}
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 256*1024)
	var reason string
	for scan.Scan() {
		line := scan.Text()
		if name, ok := strings.CutPrefix(line, "event:"); ok && strings.TrimSpace(name) == "error" {
			return "", errors.New("backend stream returned an error")
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return reason, nil
		}
		if data == "" {
			continue
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return "", fmt.Errorf("invalid backend SSE JSON: %w", err)
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			return "", errors.New("backend stream returned an error")
		}
		for _, choice := range event.Choices {
			switch choice.FinishReason {
			case "stop", "length", "content_filter":
				reason = choice.FinishReason
			case "":
			default:
				return "", errors.New("unsupported backend finish reason")
			}
			if choice.Delta.Content != "" {
				if err := emit(choice.Delta.Content); err != nil {
					return "", err
				}
			}
		}
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	return "", errors.New("backend stream ended without [DONE]")
}
