// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/automation"
	"github.com/danqing0b/cyrene-pilot-plugin/internal/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestAutomationNativeGRPC(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.jsonl")
	raw := []byte("{\"instruction\":\"Call 13800138000\",\"output\":\"ok\"}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := automation.Open(dir, automation.Config{Datasets: map[string]automation.Dataset{"data": {Path: path, Digest: automation.Digest(raw)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	wire.RegisterDirectPluginRuntimeServer(server, &runtime{binding: "pilot", automation: s})
	go func() { _ = server.Serve(l) }()
	defer server.Stop()
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := wire.NewDirectPluginRuntimeClient(conn)
	call := func(tool, args, binding string) *wire.CallToolResponse {
		t.Helper()
		raw, err := proto.Marshal(&wire.CallToolRequest{BindingId: binding, ProviderToolId: tool, ArgumentsJson: args})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Invoke(context.Background(), &wire.DirectInvocationRequest{Capability: capability, InterfaceVersion: version, Method: "call_tool", RequestId: "native-call", PayloadTypeUrl: typePrefix + "CallToolRequest", Payload: raw})
		if err != nil {
			t.Fatal(err)
		}
		var result wire.CallToolResponse
		if proto.Unmarshal(response.GetPayload().GetValue(), &result) != nil {
			t.Fatal("invalid native response")
		}
		return &result
	}
	if response := call("data_guard_start", `{"datasetId":"data","idempotencyKey":"scan"}`, "foreign"); response.GetError() == nil {
		t.Fatal("foreign binding accepted")
	}
	response := call("data_guard_start", `{"datasetId":"data","idempotencyKey":"scan"}`, "pilot")
	if response.GetError() != nil {
		t.Fatal(response.GetError())
	}
	var job automation.Job
	if json.Unmarshal([]byte(response.GetOutcome().GetContent()[0].GetJson().Json), &job) != nil {
		t.Fatal("invalid job result")
	}
	if err := s.RunPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := s.Status(job.ID)
	if err != nil || result.State != "SUCCEEDED" {
		t.Fatalf("native workflow failed: %+v %v", result, err)
	}
}

func TestAutomationOnlyConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	token := filepath.Join(dir, "operator.token")
	if err := os.WriteFile(token, []byte("operator-token-012345678901234567890123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(map[string]any{"httpListen": "127.0.0.1:0", "adminTokenFile": token, "automation": map[string]any{"models": map[string]any{}, "datasets": map[string]any{}, "dataGuard": map[string]any{}}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.Automation == nil || cfg.adminToken == "" {
		t.Fatalf("automation-only configuration rejected: %+v %v", cfg, err)
	}
}
