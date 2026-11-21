// SPDX-License-Identifier: Apache-2.0
package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type question struct {
	SampleID  string `json:"sampleId"`
	Prompt    string `json:"prompt"`
	Reference string `json:"reference"`
}
type Answer struct {
	SampleID  string `json:"sampleId"`
	Prompt    string `json:"prompt"`
	Reference string `json:"reference"`
	Actual    string `json:"actual"`
	Completion
}
type EchoReceipt struct {
	RunID       string  `json:"runId"`
	ResultID    string  `json:"resultId"`
	GateID      string  `json:"gateId"`
	InputDigest string  `json:"inputDigest"`
	Score       float64 `json:"score"`
	Passed      bool    `json:"passed"`
}

func (s *Service) evaluate(ctx context.Context, j *Job) error {
	var a EvalArgs
	if json.Unmarshal(j.Args, &a) != nil {
		return errors.New("invalid saved evaluation intent")
	}
	if _, err := s.evaluationSuite(ctx, j); err != nil {
		return err
	}
	rows, err := readDataset(s.cfg.Datasets[a.DatasetID], s.cfg.MaxRows)
	if err != nil {
		return err
	}
	questions := make([]question, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		id, idOK := row["sampleId"].(string)
		prompt, pOK := row["prompt"].(string)
		reference, rOK := row["reference"].(string)
		if !idOK || !ID.MatchString(id) || seen[id] || !pOK || strings.TrimSpace(prompt) == "" || !rOK || len(prompt) > 32768 || len(reference) > 65536 {
			return errors.New("evaluation rows require unique sampleId, bounded prompt and reference strings")
		}
		seen[id] = true
		questions = append(questions, question{id, prompt, reference})
	}
	receipts := map[string]EchoReceipt{}
	answers := map[string][]Answer{}
	for _, modelID := range []string{a.BaselineID, a.CandidateID} {
		for _, q := range questions {
			out, err := s.complete(ctx, j, fmt.Sprintf("answer:%s:%s", modelID, q.SampleID), modelID, "", q.Prompt)
			if err != nil {
				_ = s.artifactJSON(j, "partial-answers.json", answers)
				return err
			}
			answers[modelID] = append(answers[modelID], Answer{q.SampleID, q.Prompt, q.Reference, out.Text, out})
			j.Summary["completedCalls"] = len(answers[a.BaselineID]) + len(answers[a.CandidateID])
			j.Summary["totalCalls"] = len(questions) * 2
			if err := s.save(j); err != nil {
				return err
			}
		}
		var buf bytes.Buffer
		for _, answer := range answers[modelID] {
			if err := json.NewEncoder(&buf).Encode(answer); err != nil {
				return err
			}
		}
		name := "baseline.jsonl"
		if modelID == a.CandidateID {
			name = "candidate.jsonl"
		}
		if err := s.artifact(j, name, buf.Bytes()); err != nil {
			return err
		}
		r, err := s.score(ctx, j, modelID, buf.Bytes(), len(questions))
		if err != nil {
			return err
		}
		receipts[modelID] = r
	}
	base, candidate := receipts[a.BaselineID], receipts[a.CandidateID]
	regressions := []string{}
	improvements := []string{}
	for i, b := range answers[a.BaselineID] {
		c := answers[a.CandidateID][i]
		if b.Actual == b.Reference && c.Actual != c.Reference {
			regressions = append(regressions, b.SampleID)
		}
		if b.Actual != b.Reference && c.Actual == c.Reference {
			improvements = append(improvements, b.SampleID)
		}
	}
	// Per-sample exact-match changes are diagnostic only; Echo owns the selected
	// evaluator and the acceptance decision uses its score and gate.
	latencies := map[string]map[string]int64{}
	for id, items := range answers {
		values := []int64{}
		for _, a := range items {
			values = append(values, a.LatencyMS)
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		latencies[id] = map[string]int64{"p50": values[(len(values)-1)/2], "p95": values[int(math.Ceil(float64(len(values))*0.95))-1]}
	}
	j.Summary = map[string]any{"datasetDigest": s.cfg.Datasets[a.DatasetID].Digest, "sampleCount": len(questions), "baseline": base, "candidate": candidate, "accepted": candidate.Passed && candidate.Score >= base.Score, "latencyMs": latencies, "exactMatchRegressions": regressions, "exactMatchImprovements": improvements, "modelIdentityEvidence": "host-declared revision plus provider-reported model; not weight attestation"}
	return s.artifactJSON(j, "comparison.json", j.Summary)
}

// publishCAS implements the public LocalArtifactProvider single-file layout.
// Echo and the plugin must share this configured artifact mount; no cross-node
// artifact transfer or arbitrary server-side path is inferred from tool inputs.
func publishCAS(root string, data []byte) (map[string]any, error) {
	digest := Digest(data)
	hex := strings.TrimPrefix(digest, "sha256:")
	dir := filepath.Join(root, "blobs", "sha256", hex[:2])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, hex)
	if b, err := os.ReadFile(path); err == nil {
		if Digest(b) != digest {
			return nil, errors.New("CAS content integrity failure")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("CAS read unavailable")
	} else {
		f, err := os.CreateTemp(dir, ".publish-*")
		if err != nil {
			return nil, err
		}
		tmp := f.Name()
		defer os.Remove(tmp)
		_, writeErr := f.Write(data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := os.Link(tmp, path); err != nil {
			b, readErr := os.ReadFile(path)
			if readErr != nil || Digest(b) != digest {
				return nil, errors.New("CAS commit conflict")
			}
		}
	}
	return map[string]any{"uri": "artifact://sha256/" + hex, "digest": digest, "size_bytes": len(data), "kind": "dataset"}, nil
}

func (s *Service) score(ctx context.Context, j *Job, label string, data []byte, count int) (EchoReceipt, error) {
	e := s.cfg.Echo
	suite, err := s.evaluationSuite(ctx, j)
	if err != nil {
		return EchoReceipt{}, err
	}
	ref, err := publishCAS(e.ArtifactRoot, data)
	if err != nil {
		return EchoReceipt{}, errors.New("Echo shared artifact publication failed")
	}
	raw, err := s.once(j, "echo:"+label, true, func() (json.RawMessage, error) {
		return request(ctx, e.Endpoint, http.MethodPost, "/api/v1/evaluation-runs", j.ID+"-"+label, map[string]any{"suiteId": e.SuiteID, "inputArtifact": ref, "engineBindingId": e.EngineBindingID})
	})
	if err != nil {
		return EchoReceipt{}, err
	}
	var run struct {
		ID, State, SuiteID, ResultID, GateID, EngineBindingID string
		InputArtifact                                         struct{ Digest string }
	}
	if json.Unmarshal(raw, &run) != nil || run.State != "SUCCEEDED" || run.SuiteID != e.SuiteID || run.EngineBindingID != e.EngineBindingID || run.InputArtifact.Digest != ref["digest"] || !ID.MatchString(run.ID) || !ID.MatchString(run.ResultID) || !ID.MatchString(run.GateID) {
		return EchoReceipt{}, errors.New("Echo run evidence mismatch")
	}
	var result struct {
		ID, RunID, InputDigest   string
		Score                    float64
		RecordCount, PassedCount int
	}
	if err := getJSON(ctx, e.Endpoint, "/api/v1/evaluation-results/"+run.ResultID, &result); err != nil {
		return EchoReceipt{}, err
	}
	var gate struct {
		ID, RunID, ResultID, Outcome string
		ObservedScore, Threshold     float64
	}
	if err := getJSON(ctx, e.Endpoint, "/api/v1/gate-decisions/"+run.GateID, &gate); err != nil {
		return EchoReceipt{}, err
	}
	if result.ID != run.ResultID || result.RunID != run.ID || result.InputDigest != ref["digest"] || result.RecordCount != count || result.PassedCount < 0 || result.PassedCount > count || result.Score < 0 || result.Score > 1 || math.IsNaN(result.Score) || math.Abs(result.Score-float64(result.PassedCount)/float64(count)) > 1e-12 || gate.ID != run.GateID || gate.RunID != run.ID || gate.ResultID != run.ResultID || gate.ObservedScore != result.Score || gate.Threshold != suite.Threshold || (gate.Outcome != "PASS" && gate.Outcome != "FAIL") || (gate.Outcome == "PASS") != (result.Score >= gate.Threshold) {
		return EchoReceipt{}, errors.New("Echo result, coverage or gate evidence mismatch")
	}
	return EchoReceipt{run.ID, run.ResultID, run.GateID, result.InputDigest, result.Score, gate.Outcome == "PASS"}, nil
}

type suiteSnapshot struct {
	ID, Evaluator, ExpectedField, ActualField string
	Threshold                                 float64
	JudgeProfileID                            string
	ResourceVersion                           int
}

func (s *Service) evaluationSuite(ctx context.Context, j *Job) (suiteSnapshot, error) {
	var suite suiteSnapshot
	if err := getJSON(ctx, s.cfg.Echo.Endpoint, "/api/v1/evaluation-suites/"+s.cfg.Echo.SuiteID, &suite); err != nil {
		return suite, err
	}
	if suite.ID != s.cfg.Echo.SuiteID || suite.ExpectedField != "reference" || suite.ActualField != "actual" || (suite.Evaluator != "exact_match.v1" && suite.Evaluator != "llm_judge.v1") || suite.Threshold < 0 || suite.Threshold > 1 || suite.ResourceVersion < 1 {
		return suite, errors.New("Echo suite is incompatible with replay records")
	}
	raw, _ := json.Marshal(suite)
	if prior, ok := j.Steps["echo-suite"]; ok {
		if !bytes.Equal(prior.Result, raw) {
			return suite, errors.New("Echo suite changed during the comparison")
		}
	} else {
		j.Steps["echo-suite"] = Step{State: "DONE", Result: raw}
		if err := s.save(j); err != nil {
			return suite, err
		}
	}
	return suite, nil
}
