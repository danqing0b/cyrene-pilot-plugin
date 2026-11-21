// SPDX-License-Identifier: Apache-2.0
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type EvalArgs struct {
	DatasetID      string `json:"datasetId"`
	BaselineID     string `json:"baselineId"`
	CandidateID    string `json:"candidateId"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type GuardArgs struct {
	DatasetID      string `json:"datasetId"`
	IdempotencyKey string `json:"idempotencyKey"`
}
type DoctorArgs struct {
	RunID          string `json:"runId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

func (s *Service) StartEvaluation(a EvalArgs) (*Job, error) {
	if s.cfg.Echo == nil {
		return nil, errors.New("Echo is not configured")
	}
	if a.BaselineID == a.CandidateID {
		return nil, errors.New("baseline and candidate must differ")
	}
	for _, id := range []string{a.BaselineID, a.CandidateID} {
		if _, ok := s.cfg.Models[id]; !ok {
			return nil, errors.New("model is not registered")
		}
	}
	d, ok := s.cfg.Datasets[a.DatasetID]
	if !ok {
		return nil, errors.New("dataset is not registered")
	}
	if _, err := readDataset(d, s.cfg.MaxRows); err != nil {
		return nil, err
	}
	return s.enqueue("evaluation", a.IdempotencyKey, a)
}
func (s *Service) StartGuard(a GuardArgs) (*Job, error) {
	d, ok := s.cfg.Datasets[a.DatasetID]
	if !ok {
		return nil, errors.New("dataset is not registered")
	}
	if _, err := readDataset(d, s.cfg.MaxRows); err != nil {
		return nil, err
	}
	return s.enqueue("data_guard", a.IdempotencyKey, a)
}
func (s *Service) StartDoctor(a DoctorArgs) (*Job, error) {
	if s.cfg.Yield == nil {
		return nil, errors.New("Yield is not configured")
	}
	allowed := false
	for _, id := range s.cfg.Yield.AllowedRunIDs {
		allowed = allowed || id == a.RunID
	}
	if !allowed {
		return nil, errors.New("run is not registered by the host")
	}
	return s.enqueue("run_doctor", a.IdempotencyKey, a)
}
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = s.RunPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
func (s *Service) RunPending(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.jobs()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State != "QUEUED" {
			continue
		}
		if j.ConfigDigest != s.cfg.fingerprint() {
			j.State = "NEEDS_ATTENTION"
			j.Summary["error"] = "HOST_CONFIGURATION_CHANGED"
			return s.save(j)
		}
		j.State = "RUNNING"
		if err := s.save(j); err != nil {
			return err
		}
		switch j.Kind {
		case "evaluation":
			err = s.evaluate(ctx, j)
		case "data_guard":
			err = s.guard(ctx, j)
		case "run_doctor":
			err = s.diagnose(ctx, j)
		default:
			err = errors.New("unknown job kind")
		}
		if err != nil {
			j.State = "FAILED"
			for _, step := range j.Steps {
				if step.State == "UNKNOWN" || step.State == "IN_FLIGHT" {
					j.State = "NEEDS_ATTENTION"
				}
			}
			// All workflow errors are bounded local messages, never raw HTTP/model output.
			j.Summary["error"] = err.Error()
		} else {
			j.State = "SUCCEEDED"
		}
		return s.save(j)
	}
	return nil
}

// once persists dispatch intent before the side effect. Only endpoints with an
// audited idempotency contract may be retried after an interrupted dispatch.
func (s *Service) once(j *Job, label string, replaySafe bool, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	prior, ok := j.Steps[label]
	if ok && prior.State == "DONE" {
		return prior.Result, nil
	}
	if ok && !replaySafe {
		return nil, errors.New("operation outcome is unknown or failed; automatic replay is blocked")
	}
	j.Steps[label] = Step{State: "IN_FLIGHT"}
	if err := s.save(j); err != nil {
		return nil, err
	}
	raw, err := fn()
	if err != nil {
		code, state := "REMOTE_OUTCOME_UNKNOWN", "UNKNOWN"
		var h remoteError
		if errors.As(err, &h) && h.status >= 400 && h.status < 500 && h.status != 408 {
			code, state = "REMOTE_REJECTED", "FAILED"
		}
		j.Steps[label] = Step{State: state, Code: code}
		if e := s.save(j); e != nil {
			return nil, e
		}
		return nil, err
	}
	j.Steps[label] = Step{State: "DONE", Result: raw}
	if err := s.save(j); err != nil {
		return nil, err
	}
	return raw, nil
}
func (s *Service) artifactJSON(j *Job, name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.artifact(j, name, raw)
}

// Review actions are exposed through the same native tool protocol and the
// authenticated operator HTTP surface. The host budget controls compute starts.
func (s *Service) Action(ctx context.Context, id, action, planDigest string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if j.State != "SUCCEEDED" || j.ConfigDigest != s.cfg.fingerprint() {
		return nil, errors.New("job is not a completed job under the current host configuration")
	}
	switch action {
	case "guard_apply":
		err = s.applyGuard(ctx, j, planDigest)
	case "doctor_prepare":
		err = s.prepareRepair(ctx, j, planDigest)
	case "doctor_start":
		err = s.startRepair(ctx, j, planDigest)
	case "doctor_check":
		err = s.checkRepair(ctx, j)
	default:
		return nil, errors.New("unsupported action")
	}
	if err != nil {
		return nil, err
	}
	return s.Status(id)
}

type Tool struct{ ID, Name, Description, Schema string }

const keySchema = `"idempotencyKey":{"type":"string","minLength":1,"maxLength":128}`

func (s *Service) Tools() []Tool {
	tools := []Tool{
		{"pilot_job_status", "Read automation job", "Read state, receipts and artifact digests.", `{"type":"object","properties":{"jobId":{"type":"string"}},"required":["jobId"],"additionalProperties":false}`},
		{"data_guard_start", "Scan and redact dataset", "Create a review draft using rules and optional host-authorized AI.", `{"type":"object","properties":{"datasetId":{"type":"string"},` + keySchema + `},"required":["datasetId","idempotencyKey"],"additionalProperties":false}`},
	}
	if s.cfg.Echo != nil {
		tools = append(tools, Tool{"eval_replay_start", "Collect and compare model answers", "Run both registered models against the same frozen questions and score with Echo.", `{"type":"object","properties":{"datasetId":{"type":"string"},"baselineId":{"type":"string"},"candidateId":{"type":"string"},` + keySchema + `},"required":["datasetId","baselineId","candidateId","idempotencyKey"],"additionalProperties":false}`})
	}
	if s.cfg.Yield != nil {
		tools = append(tools, Tool{"run_doctor_start", "Diagnose a failed run", "Collect Yield evidence and produce a bounded, digest-bound repair plan; does not start training.", `{"type":"object","properties":{"runId":{"type":"string"},` + keySchema + `},"required":["runId","idempotencyKey"],"additionalProperties":false}`})
	}
	actions := []string{"doctor_prepare", "doctor_start", "doctor_check", "guard_apply"}
	for _, action := range actions {
		if action == "guard_apply" && s.cfg.Catalyst == nil {
			continue
		}
		if action != "guard_apply" && s.cfg.Yield == nil {
			continue
		}
		tools = append(tools, Tool{"pilot_" + action, "Apply reviewed workflow action", "Requires the saved plan digest for writes; compute starts also require a host budget.", `{"type":"object","properties":{"jobId":{"type":"string"},"planDigest":{"type":"string"}},"required":["jobId"],"additionalProperties":false}`})
	}
	return tools
}
func (s *Service) Call(ctx context.Context, tool string, raw []byte) (any, error) {
	if len(raw) > MaxReply {
		return nil, errors.New("arguments too large")
	}
	switch tool {
	case "eval_replay_start":
		var a EvalArgs
		if err := Strict(raw, &a); err != nil {
			return nil, err
		}
		return s.StartEvaluation(a)
	case "data_guard_start":
		var a GuardArgs
		if err := Strict(raw, &a); err != nil {
			return nil, err
		}
		return s.StartGuard(a)
	case "run_doctor_start":
		var a DoctorArgs
		if err := Strict(raw, &a); err != nil {
			return nil, err
		}
		return s.StartDoctor(a)
	case "pilot_job_status":
		var a struct {
			JobID string `json:"jobId"`
		}
		if err := Strict(raw, &a); err != nil {
			return nil, err
		}
		return s.Status(a.JobID)
	default:
		var a struct {
			JobID      string `json:"jobId"`
			PlanDigest string `json:"planDigest"`
		}
		if err := Strict(raw, &a); err != nil {
			return nil, err
		}
		for _, action := range []string{"guard_apply", "doctor_prepare", "doctor_start", "doctor_check"} {
			if tool == "pilot_"+action {
				return s.Action(ctx, a.JobID, action, a.PlanDigest)
			}
		}
		return nil, errors.New("unknown automation tool")
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.Status(r.PathValue("id"))
		respond(w, j, err)
	})
	mux.HandleFunc("GET /admin/jobs/{id}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.ReadArtifact(r.PathValue("id"), r.PathValue("name"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("POST /admin/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxReply))
		if err != nil || !json.Valid(raw) {
			respond(w, nil, errors.New("invalid JSON"))
			return
		}
		v, err := s.Call(r.Context(), r.PathValue("tool"), raw)
		respond(w, v, err)
	})
	return mux
}
func respond(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		code := http.StatusUnprocessableEntity
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		if errors.Is(err, ErrConflict) {
			code = 409
		}
		w.WriteHeader(code)
		v = map[string]string{"error": fmt.Sprint(err)}
	}
	_ = json.NewEncoder(w).Encode(v)
}
