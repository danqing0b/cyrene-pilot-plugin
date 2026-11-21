// SPDX-License-Identifier: Apache-2.0
package main

import (
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

var errWaiting = errors.New("waiting for downstream state")

type permanentError string

func (e permanentError) Error() string { return string(e) }

type httpError struct{ status int }

func (e httpError) Error() string { return fmt.Sprintf("downstream HTTP %d", e.status) }

func terminal(err error) bool {
	var problem httpError
	var invalid permanentError
	return errors.As(err, &invalid) ||
		(errors.As(err, &problem) && problem.status >= 400 && problem.status < 500 &&
			problem.status != 408 && problem.status != 429)
}

type products struct {
	cfg    config
	client *http.Client
}

func newProducts(cfg config) *products {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &products{cfg: cfg, client: &http.Client{
		Transport: transport, Timeout: 50 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (p *products) raw(ctx context.Context, conn connection, method, path, key string, body any) ([]byte, string, error) {
	var data io.Reader
	if body != nil {
		value, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		data = bytes.NewReader(value)
	}
	req, err := http.NewRequestWithContext(ctx, method, conn.URL+path, data)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if conn.token != "" {
		req.Header.Set("Authorization", "Bearer "+conn.token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, "", errors.New("downstream request unavailable")
	}
	defer res.Body.Close()
	requestID := res.Header.Get("X-Request-Id")
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, requestID, httpError{res.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, requestID, errors.New("could not read downstream response")
	}
	if len(raw) > maxBody {
		return nil, requestID, permanentError("downstream response exceeds limit")
	}
	return raw, requestID, nil
}

func (p *products) json(ctx context.Context, conn connection, method, path, key string, body, out any) error {
	raw, _, err := p.raw(ctx, conn, method, path, key, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return permanentError("invalid downstream JSON")
	}
	return nil
}

func resource(prefix, id, suffix string) string { return prefix + url.PathEscape(id) + suffix }

type evaluationRun struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	SuiteID  string `json:"suiteId"`
	ResultID string `json:"resultId"`
	GateID   string `json:"gateId"`
}
type evaluationReport struct {
	Schema   string `json:"schemaVersion"`
	ResultID string `json:"resultId"`
	Target   struct {
		Dataset string `json:"versionRef"`
		Digest  string `json:"packageDigest"`
	} `json:"target"`
	Evaluator struct {
		ID string `json:"id"`
	} `json:"evaluator"`
	Coverage struct {
		Total     int
		Evaluated int
		Failed    int
		Skipped   int
	} `json:"coverage"`
	Metrics []struct {
		Name      string
		Value     float64
		Evaluated int
		Matched   int
	} `json:"metrics"`
	Samples []struct {
		ID     string `json:"sampleId"`
		Status string `json:"status"`
	} `json:"samples"`
}

func (p *products) evaluate(ctx context.Context, input, digest, key string) (assessment, error) {
	var run evaluationRun
	err := p.json(ctx, p.cfg.Echo, http.MethodPost, resource("/api/v1/evaluation-inputs/", input, "/actions/evaluate"),
		key, map[string]string{"suiteId": p.cfg.SuiteID, "engineBindingId": "exact-match-plugin"}, &run)
	if err != nil {
		return assessment{}, err
	}
	if run.State != "SUCCEEDED" || run.SuiteID != p.cfg.SuiteID {
		return assessment{}, permanentError("Echo evaluation did not succeed with the selected suite")
	}
	return p.assess(ctx, run, digest)
}

func (p *products) assess(ctx context.Context, run evaluationRun, digest string) (assessment, error) {
	var result assessment
	if run.State != "SUCCEEDED" || !identifier.MatchString(run.ID) || !identifier.MatchString(run.GateID) ||
		!identifier.MatchString(run.ResultID) {
		return result, permanentError("Echo did not return valid evaluation evidence")
	}
	var gate struct {
		ID       string  `json:"id"`
		RunID    string  `json:"runId"`
		ResultID string  `json:"resultId"`
		Outcome  string  `json:"outcome"`
		Score    float64 `json:"observedScore"`
	}
	if err := p.json(ctx, p.cfg.Echo, http.MethodGet, resource("/api/v1/gate-decisions/", run.GateID, ""), "", nil, &gate); err != nil {
		return result, err
	}
	var report evaluationReport
	if err := p.json(ctx, p.cfg.Echo, http.MethodGet, resource("/api/v1/evaluation-results/", run.ResultID, "/export"), "", nil, &report); err != nil {
		return result, err
	}
	if report.Schema != "cyrene.echo.evaluation-report.v1" || report.ResultID != run.ResultID ||
		report.Target.Digest != digest || !strings.HasPrefix(report.Target.Dataset, "catalyst://") ||
		report.Evaluator.ID != "exact_match.v1" || gate.ID != run.GateID || gate.RunID != run.ID || gate.ResultID != run.ResultID ||
		len(report.Metrics) != 1 || report.Metrics[0].Name != "exact_match" || report.Metrics[0].Value != gate.Score ||
		(gate.Outcome != "PASS" && gate.Outcome != "FAIL") {
		return result, permanentError("Echo evidence does not match the selected model, evaluator, or run")
	}
	coverage := report.Coverage
	if coverage.Evaluated == 0 || coverage.Total != coverage.Evaluated ||
		coverage.Failed != 0 || coverage.Skipped != 0 || len(report.Samples) != coverage.Total ||
		report.Metrics[0].Evaluated != coverage.Evaluated {
		return result, permanentError("evaluation coverage is incomplete")
	}
	seen := make(map[string]bool)
	for i, sample := range report.Samples {
		if sample.ID == "" || sample.Status != "EVALUATED" || seen[sample.ID] {
			return result, permanentError("evaluation sample identities are invalid")
		}
		seen[sample.ID] = true
		result.SampleIDs = append(result.SampleIDs, sample.ID)
		result.EvaluatedIndexes = append(result.EvaluatedIndexes, i+1)
	}
	result.RunID, result.GateID, result.ResultID = run.ID, run.GateID, run.ResultID
	result.Dataset, result.Digest, result.Score, result.Pass = report.Target.Dataset, digest, gate.Score, gate.Outcome == "PASS"
	return result, nil
}

type deployment struct {
	ID         string   `json:"id"`
	EndpointID string   `json:"endpointId"`
	State      string   `json:"observedState"`
	Artifact   artifact `json:"modelArtifact"`
}

func (p *products) deploy(ctx context.Context, r *release) (deployment, error) {
	var d deployment
	var err error
	if r.DeploymentID == "" {
		err = p.json(ctx, p.cfg.Reactor, http.MethodPost, "/api/v1/deployments", r.ID+"-deploy",
			map[string]any{"name": "release-pilot-" + r.ID, "modelArtifact": r.Candidate.Model,
				"servingBindingId": r.Candidate.ServingBinding}, &d)
	} else {
		err = p.json(ctx, p.cfg.Reactor, http.MethodGet, resource("/api/v1/deployments/", r.DeploymentID, ""), "", nil, &d)
	}
	if err != nil {
		return d, err
	}
	if !identifier.MatchString(d.ID) || (r.DeploymentID != "" && r.DeploymentID != d.ID) ||
		d.Artifact != r.Candidate.Model {
		return d, permanentError("Reactor deployment identity or model artifact changed")
	}
	return d, nil
}

type endpoint struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deploymentId"`
	State        string `json:"state"`
	Model        string `json:"model"`
	Version      int    `json:"resourceVersion"`
}

func (p *products) endpoint(ctx context.Context, id, deploymentID string) (endpoint, error) {
	var e endpoint
	if err := p.json(ctx, p.cfg.Reactor, http.MethodGet, resource("/api/v1/endpoints/", id, ""), "", nil, &e); err != nil {
		return e, err
	}
	if e.ID != id || e.DeploymentID != deploymentID || e.Model == "" || e.Version <= 0 {
		return e, permanentError("Reactor endpoint identity has changed")
	}
	if e.State != "READY" {
		return e, errWaiting
	}
	return e, nil
}

type exchangeRoute struct {
	ID       string `json:"id"`
	Alias    string `json:"modelPattern"`
	Model    string `json:"targetModel"`
	State    string `json:"state"`
	Binding  string `json:"targetBindingId"`
	Endpoint string `json:"endpointId"`
	Version  int    `json:"resourceVersion"`
	Source   struct {
		Digest string `json:"artifactDigest"`
	} `json:"source"`
}

func (r exchangeRoute) target() route {
	return route{ID: r.ID, Alias: r.Alias, Model: r.Model, Version: r.Version}
}

func (p *products) draft(ctx context.Context, r *release, e endpoint) (exchangeRoute, error) {
	var response struct {
		Route exchangeRoute `json:"route"`
	}
	err := p.json(ctx, p.cfg.Reactor, http.MethodPost, resource("/api/v1/endpoints/", e.ID, "/actions/send-to-exchange"), r.ID+"-route",
		map[string]any{"resourceVersion": e.Version, "receiverId": p.cfg.ReceiverID, "gatewayEndpointId": p.cfg.GatewayID,
			"targetBindingId": r.Candidate.ProviderBinding, "modelPattern": "release-pilot-" + r.ID, "priority": 100}, &response)
	if err != nil {
		return exchangeRoute{}, err
	}
	d := response.Route
	if !identifier.MatchString(d.ID) || d.Version < 1 || d.State != "DRAFT" || d.Model != e.Model || !p.routeMatches(d, r) {
		return d, permanentError("Exchange draft does not match candidate endpoint")
	}
	return d, nil
}

func (p *products) routeMatches(d exchangeRoute, r *release) bool {
	return d.Alias == "release-pilot-"+r.ID && d.Endpoint == p.cfg.GatewayID &&
		d.Binding == r.Candidate.ProviderBinding && d.Source.Digest == r.Candidate.Model.Digest
}

func (p *products) readRoute(ctx context.Context, id string) (exchangeRoute, error) {
	var d exchangeRoute
	err := p.json(ctx, p.cfg.Exchange, http.MethodGet, resource("/api/v1/gateway-routes/", id, ""), "", nil, &d)
	if err == nil && d.ID != id {
		return d, permanentError("unexpected Exchange route identity")
	}
	return d, err
}

func (p *products) activate(ctx context.Context, r *release) (route, error) {
	d, err := p.readRoute(ctx, r.DraftID)
	if err != nil {
		return route{}, err
	}
	if !p.routeMatches(d, r) || d.Model != r.Target.Model {
		return route{}, permanentError("Exchange draft changed after inspection")
	}
	if d.State == "DRAFT" && d.Version == r.Target.Version {
		err = p.json(ctx, p.cfg.Exchange, http.MethodPost,
			resource("/api/v1/gateway-route-drafts/", d.ID, "/actions/confirm"), "",
			map[string]int{"resourceVersion": d.Version}, &d)
		if err != nil {
			observed, lookupErr := p.readRoute(ctx, r.DraftID)
			if lookupErr != nil || observed.State != "ACTIVE" {
				return route{}, err
			}
			d = observed
		}
	}
	if d.State != "ACTIVE" || d.Version != r.Target.Version+1 ||
		d.ID != r.DraftID || d.Model != r.Target.Model || !p.routeMatches(d, r) {
		return route{}, permanentError("Exchange route activation could not be verified")
	}
	return d.target(), nil
}

func (p *products) probe(ctx context.Context, r route) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var routes []exchangeRoute
	if err := p.json(ctx, p.cfg.Exchange, http.MethodGet, "/api/v1/gateway-routes", "", nil, &routes); err != nil {
		return "", err
	}
	found := false
	for _, candidate := range routes {
		if candidate.State != "ACTIVE" || candidate.Endpoint != p.cfg.GatewayID {
			continue
		}
		if strings.ContainsAny(candidate.Alias, "*?[") {
			return "", permanentError("the isolated pilot gateway requires exact aliases")
		}
		if candidate.Alias == r.Alias {
			if found || candidate.ID != r.ID || candidate.Model != r.Model ||
				(r.Version != 0 && r.Version != candidate.Version) {
				return "", permanentError("the Exchange alias no longer selects the exact pilot route")
			}
			found = true
		}
	}
	if !found {
		return "", errWaiting
	}
	body := map[string]any{"model": r.Alias, "max_tokens": 8,
		"messages": []map[string]string{{"role": "user", "content": "Return the label A."}}}
	data, requestID, err := p.raw(ctx, p.cfg.Inference, http.MethodPost, "/v1/chat/completions", "", body)
	if err != nil {
		return requestID, err
	}
	var result struct {
		Model   string
		Choices []struct{ Message struct{ Content string } }
	}
	if json.Unmarshal(data, &result) != nil || result.Model != r.Alias ||
		len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		return requestID, errors.New("unexpected model or empty inference result")
	}
	return requestID, nil
}
