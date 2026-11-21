// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryKeepsDeploymentIntentAfterTransportLoss(t *testing.T) {
	t.Parallel()
	item := candidate{Model: artifact{Kind: "model", Size: 1}, ServingBinding: "binding"}
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deployments" ||
			r.Header.Get("Idempotency-Key") != "release-id-deploy" {
			t.Error("unexpected deployment intent")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if sends.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			// The Product accepted the operation, then lost its response.
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(deployment{ID: "original-deployment", State: "READY", Artifact: item.Model})
	}))
	defer server.Close()
	p := newProducts(config{Reactor: connection{URL: server.URL}})
	intent := &release{ID: "release-id", Candidate: item}
	if _, err := p.deploy(context.Background(), intent); err == nil || terminal(err) {
		t.Fatalf("expected an uncertain transport result, got %v", err)
	}
	result, err := p.deploy(context.Background(), intent)
	if err != nil || result.ID != "original-deployment" || sends.Load() != 2 {
		t.Fatalf("retry did not preserve the original operation: %v %v", result, err)
	}
}

func TestMissedMonitoringWindowDoesNotPromoteCandidate(t *testing.T) {
	t.Parallel()
	target := route{ID: "next-route", Alias: "exact-alias", Model: "next-model", Version: 2}
	const gatewayID = "pilot-gateway"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/gateway-routes":
			_ = json.NewEncoder(w).Encode([]exchangeRoute{
				{ID: target.ID, Alias: target.Alias, Model: target.Model, Version: target.Version, State: "ACTIVE", Endpoint: gatewayID},
			})
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"model":"exact-alias","choices":[{"message":{"content":"A"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	db, err := openStore(t.TempDir(), baselineConfig{RouteID: "old-route", Digest: "old-digest"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	cfg := config{Exchange: connection{URL: api.URL}, Inference: connection{URL: api.URL}, GatewayID: gatewayID}
	then := time.Unix(1_000, 0)
	now := then.Add(5 * time.Minute)
	c := coordinator{store: db, product: newProducts(cfg), now: func() time.Time { return now }}
	if err := db.update(func(s *state) error {
		s.Active, s.Version, s.JobID = target, 2, "release"
		s.Releases[s.JobID] = &release{
			ID: "release", State: monitoring, Target: target, SwitchedVersion: 2,
			StableSince: then, NextProbe: then.Add(probeInterval),
			Candidate: candidate{InputID: "next-input", Model: artifact{Digest: "next-digest"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.tick(context.Background())
	s, err := db.view()
	if err != nil || s.Releases["release"].State != monitoring || !s.Releases["release"].StableSince.Equal(now) {
		t.Fatalf("a single probe after downtime was counted as a stable window: %v %v", s, err)
	}
	for range 12 {
		now = now.Add(probeInterval)
		c.tick(context.Background())
	}
	s, err = db.view()
	if err != nil || s.Releases["release"].State != released || s.JobID != "" ||
		s.Baseline.RouteID != target.ID || s.Baseline.Digest != "next-digest" {
		t.Fatalf("stable candidate was not promoted to the persisted baseline: %v %v", s, err)
	}
}
