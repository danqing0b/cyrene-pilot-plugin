// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/wire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const capability = "tool.provider.v1"
const version = "1"
const typePrefix = "type.cyrene.io/cyrene.tool.provider.v1."

type runtime struct {
	wire.UnimplementedDirectPluginRuntimeServer
	binding     string
	coordinator *coordinator
}

func (s *runtime) Health(context.Context, *wire.HealthRequest) (*wire.HealthResponse, error) {
	return &wire.HealthResponse{Status: wire.HealthResponse_STATUS_SERVING,
		PluginId: pluginID, PluginVersion: pluginVersion, Capabilities: []string{capability}}, nil
}

func (s *runtime) InvokeStream(*wire.DirectInvocationRequest, wire.DirectPluginRuntime_InvokeStreamServer) error {
	return status.Error(codes.Unimplemented, "tool.provider.v1 supports unary invocation")
}

func invalidRequest(code wire.DirectInvocationError_Code, text string) (*wire.DirectInvocationResponse, error) {
	return &wire.DirectInvocationResponse{Result: &wire.DirectInvocationResponse_Error{
		Error: &wire.DirectInvocationError{Code: code, Message: text}}}, nil
}

func (s *runtime) Invoke(_ context.Context, req *wire.DirectInvocationRequest) (*wire.DirectInvocationResponse, error) {
	if req.GetCapability() != capability || req.GetInterfaceVersion() != version ||
		strings.TrimSpace(req.GetRequestId()) == "" || len(req.GetRequestId()) > 256 ||
		req.GetStreamMode() != wire.DirectStreamMode_DIRECT_STREAM_MODE_UNSPECIFIED ||
		len(req.GetPayload()) > maxBody {
		return invalidRequest(wire.DirectInvocationError_CODE_INVALID_REQUEST, "invalid invocation envelope")
	}
	var input proto.Message
	var suffix string
	switch req.Method {
	case "list_tools":
		input, suffix = &wire.ListToolsRequest{}, "ListTools"
	case "call_tool":
		input, suffix = &wire.CallToolRequest{}, "CallTool"
	default:
		return invalidRequest(wire.DirectInvocationError_CODE_METHOD_NOT_FOUND, "unsupported method")
	}
	if req.PayloadTypeUrl != typePrefix+suffix+"Request" || proto.Unmarshal(req.Payload, input) != nil {
		return invalidRequest(wire.DirectInvocationError_CODE_INVALID_REQUEST, "invalid typed payload")
	}
	var output proto.Message
	switch value := input.(type) {
	case *wire.ListToolsRequest:
		output = s.listTools(value.GetBindingId())
	case *wire.CallToolRequest:
		output = s.callTool(value)
	}
	raw, err := proto.Marshal(output)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to encode tool response")
	}
	return &wire.DirectInvocationResponse{Result: &wire.DirectInvocationResponse_Payload{Payload: &wire.DirectPayload{
		TypeUrl: typePrefix + suffix + "Response", Value: raw}}}, nil
}

const prepareSchema = `{"type":"object","properties":{"candidateId":{"type":"string"},"idempotencyKey":{"type":"string"}},"required":["candidateId","idempotencyKey"],"additionalProperties":false}`
const statusSchema = `{"type":"object","properties":{"releaseId":{"type":"string"}},"required":["releaseId"],"additionalProperties":false}`

func providerError(code wire.ToolProviderErrorCode, message string) *wire.ToolProviderError {
	return &wire.ToolProviderError{Code: code, Message: message}
}

func (s *runtime) listTools(binding string) *wire.ListToolsResponse {
	if binding != "" && binding != s.binding {
		return &wire.ListToolsResponse{Result: &wire.ListToolsResponse_Error{
			Error: providerError(wire.ToolProviderErrorCode_TOOL_PROVIDER_ERROR_CODE_INVALID_ARGUMENTS, "unknown binding")}}
	}
	return &wire.ListToolsResponse{Result: &wire.ListToolsResponse_Catalog{Catalog: &wire.ToolCatalog{
		CatalogVersion: "release-pilot-v1", Tools: []*wire.ToolDescriptor{
			{BindingId: s.binding, ProviderToolId: "release_prepare", DisplayName: "Prepare model release", InputSchemaJson: prepareSchema,
				Description: "Evaluate and stage a registered candidate for operator approval."},
			{BindingId: s.binding, ProviderToolId: "release_status", DisplayName: "Read model release status", InputSchemaJson: statusSchema,
				Description: "Read the state and durable receipts for a release."},
		}}}}
}

func toolError(code wire.ToolProviderErrorCode, message string) *wire.CallToolResponse {
	return &wire.CallToolResponse{Result: &wire.CallToolResponse_Error{Error: providerError(code, message)}}
}

func (s *runtime) callTool(req *wire.CallToolRequest) *wire.CallToolResponse {
	badInput := wire.ToolProviderErrorCode_TOOL_PROVIDER_ERROR_CODE_INVALID_ARGUMENTS
	if req.BindingId != s.binding {
		return toolError(badInput, "unknown binding")
	}
	var entry *release
	var err error
	switch req.ProviderToolId {
	case "release_prepare":
		var args struct {
			CandidateID string `json:"candidateId"`
			Key         string `json:"idempotencyKey"`
		}
		if strictJSON(strings.NewReader(req.ArgumentsJson), &args) != nil {
			return toolError(badInput, "invalid release arguments")
		}
		entry, err = s.coordinator.prepare(args.CandidateID, args.Key)
	case "release_status":
		var args struct {
			ID string `json:"releaseId"`
		}
		if strictJSON(strings.NewReader(req.ArgumentsJson), &args) != nil || !identifier.MatchString(args.ID) {
			return toolError(badInput, "invalid releaseId")
		}
		var snapshot state
		snapshot, err = s.coordinator.store.view()
		entry = snapshot.Releases[args.ID]
		if entry == nil && err == nil {
			err = errNotFound
		}
	default:
		return toolError(wire.ToolProviderErrorCode_TOOL_PROVIDER_ERROR_CODE_TOOL_NOT_FOUND, "unknown tool")
	}
	if err != nil {
		if errors.Is(err, errConflict) || errors.Is(err, errNotFound) || terminal(err) {
			return toolError(badInput, err.Error())
		}
		return toolError(wire.ToolProviderErrorCode_TOOL_PROVIDER_ERROR_CODE_EXECUTION_FAILED, "release store unavailable")
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return toolError(wire.ToolProviderErrorCode_TOOL_PROVIDER_ERROR_CODE_EXECUTION_FAILED, "release could not be encoded")
	}
	return &wire.CallToolResponse{Result: &wire.CallToolResponse_Outcome{Outcome: &wire.ToolCallOutcome{
		Content: []*wire.ToolContentPart{{Content: &wire.ToolContentPart_Json{Json: &wire.ToolJsonContent{Json: string(data)}}}},
	}}}
}
