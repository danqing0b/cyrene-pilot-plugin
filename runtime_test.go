// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/wire"
	"google.golang.org/protobuf/proto"
)

func TestNativeToolProtocol(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{
		Baseline:   baselineConfig{RouteID: "baseline", Alias: "known-baseline", Model: "baseline"},
		Candidates: map[string]candidate{"tested": {Model: artifact{Digest: "candidate-digest"}}},
	}
	db, err := openStore(dir, cfg.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.db.Close() })
	h := &runtime{binding: "test-binding", coordinator: &coordinator{
		store: db, product: newProducts(cfg), now: time.Now, wake: make(chan struct{}, 1),
	}}
	body, err := proto.Marshal(&wire.ListToolsRequest{BindingId: proto.String("test-binding")})
	if err != nil {
		t.Fatal(err)
	}
	req := &wire.DirectInvocationRequest{Capability: capability, InterfaceVersion: version,
		Method: "list_tools", PayloadTypeUrl: typePrefix + "ListToolsRequest", Payload: body, RequestId: "request"}
	resp, err := h.Invoke(context.Background(), req)
	var list wire.ListToolsResponse
	if err != nil || resp.GetPayload().GetTypeUrl() != typePrefix+"ListToolsResponse" ||
		proto.Unmarshal(resp.GetPayload().GetValue(), &list) != nil || len(list.GetCatalog().GetTools()) != 2 {
		t.Fatalf("invalid catalog: %v %v", resp, err)
	}
	req.PayloadTypeUrl = typePrefix + "CallToolRequest"
	resp, err = h.Invoke(context.Background(), req)
	if err != nil || resp.GetError().GetCode() != wire.DirectInvocationError_CODE_INVALID_REQUEST {
		t.Fatalf("incorrect type URL accepted: %v %v", resp, err)
	}
	if result := h.callTool(&wire.CallToolRequest{BindingId: "foreign", ProviderToolId: "release_prepare"}); result.GetError() == nil {
		t.Fatal("foreign binding accepted")
	}
	if result := h.callTool(&wire.CallToolRequest{BindingId: "test-binding", ProviderToolId: "release_approve"}); result.GetError() == nil {
		t.Fatal("approval unexpectedly exposed to the tool client")
	}
}

func TestConcurrentPrepareAndRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := baselineConfig{InputID: "baseline-input", RouteID: "baseline-route", Alias: "baseline-alias", Model: "baseline"}
	cfg := config{Baseline: base, Candidates: map[string]candidate{"candidate": {Model: artifact{Digest: "candidate-digest"}}}}
	db, err := openStore(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	c := coordinator{store: db, product: newProducts(cfg), now: time.Now}
	ids := make(chan string, 16)
	var wg sync.WaitGroup
	for range cap(ids) {
		wg.Go(func() {
			r, err := c.prepare("candidate", "one-intent")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.ID
		})
	}
	wg.Wait()
	close(ids)
	var expected string
	for id := range ids {
		if expected != "" && expected != id {
			t.Fatal("concurrent duplicate created two release IDs")
		}
		expected = id
	}
	if err := db.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openStore(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.db.Close()
	c.store = db
	actual, err := c.prepare("candidate", "one-intent")
	if err != nil || actual.ID != expected || actual.Reference.InputID != base.InputID {
		t.Fatalf("restart changed the durable intent: %v %v", actual, err)
	}
	state, err := db.view()
	if err != nil || len(state.Releases) != 1 || state.Active.ID != base.RouteID {
		t.Fatalf("unexpected durable state: %v %v", state, err)
	}
}

func TestStrictToolArguments(t *testing.T) {
	t.Parallel()
	var input struct{ ID string }
	for _, raw := range []string{`{"ID":"candidate","token":"unexpected"}`, `{"ID":"candidate"} {}`} {
		if err := strictJSON(strings.NewReader(raw), &input); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if err := strictJSON(strings.NewReader(`{"ID":"candidate"}`), &input); err != nil {
		t.Fatal(err)
	}
}
