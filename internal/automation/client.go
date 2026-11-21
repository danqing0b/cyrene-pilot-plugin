// SPDX-License-Identifier: Apache-2.0
package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type remoteError struct{ status int }

func (e remoteError) Error() string { return fmt.Sprintf("downstream returned HTTP %d", e.status) }

func request(ctx context.Context, e Endpoint, method, path, key string, body any) (json.RawMessage, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, e.URL+path, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid fixed endpoint request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 130 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("downstream transport outcome unknown")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, remoteError{res.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxReply+1))
	if err != nil || len(b) > MaxReply {
		return nil, errors.New("downstream response unreadable or too large")
	}
	if !json.Valid(b) {
		return nil, errors.New("downstream returned invalid JSON")
	}
	return b, nil
}
func getJSON(ctx context.Context, e Endpoint, path string, out any) error {
	raw, err := request(ctx, e, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid downstream resource")
	}
	return nil
}

type Completion struct {
	Text             string           `json:"text"`
	Model            string           `json:"model"`
	Revision         string           `json:"hostDeclaredRevision"`
	LatencyMS        int64            `json:"latencyMs"`
	Usage            map[string]int64 `json:"usage,omitempty"`
	GenerationDigest string           `json:"generationDigest"`
}

func (s *Service) complete(ctx context.Context, j *Job, label, modelID, system, prompt string) (Completion, error) {
	m, ok := s.cfg.Models[modelID]
	if !ok {
		return Completion{}, errors.New("unknown configured model")
	}
	if system == "" {
		system = m.SystemPrompt
	}
	raw, err := s.once(j, label, false, func() (json.RawMessage, error) {
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
		defer cancel()
		messages := []map[string]string{}
		if system != "" {
			messages = append(messages, map[string]string{"role": "system", "content": system})
		}
		messages = append(messages, map[string]string{"role": "user", "content": prompt})
		started := time.Now()
		b, err := request(callCtx, m.Endpoint, http.MethodPost, "/v1/chat/completions", "", map[string]any{"model": m.Model, "messages": messages, "temperature": 0, "max_tokens": m.MaxTokens, "stream": false})
		if err != nil {
			return nil, err
		}
		var response struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]int64 `json:"usage"`
		}
		if json.Unmarshal(b, &response) != nil || response.Model != m.Model || len(response.Choices) != 1 || response.Choices[0].Message.Content == nil || response.Choices[0].FinishReason != "stop" {
			return nil, errors.New("model returned an incomplete or mismatched response")
		}
		text := *response.Choices[0].Message.Content
		if len(text) > 65536 || text == "" {
			return nil, errors.New("model returned empty or oversized content")
		}
		usage := map[string]int64{}
		for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if n, ok := response.Usage[k]; ok && n >= 0 {
				usage[k] = n
			}
		}
		generation, _ := json.Marshal(map[string]any{"messages": messages, "model": m.Model, "temperature": 0, "max_tokens": m.MaxTokens})
		return json.Marshal(Completion{Text: text, Model: response.Model, Revision: m.Revision, LatencyMS: time.Since(started).Milliseconds(), Usage: usage, GenerationDigest: Digest(generation)})
	})
	var out Completion
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil {
		return out, errors.New("saved completion is invalid")
	}
	return out, nil
}
