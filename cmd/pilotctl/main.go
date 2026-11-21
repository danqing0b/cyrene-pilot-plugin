// SPDX-License-Identifier: Apache-2.0
// pilotctl is a native DirectPluginRuntime client, useful before host registration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func run() error {
	ref := flag.String("connection", "", "Platform-resolved local connection_ref")
	binding := flag.String("binding", "pilot", "Capability binding ID")
	tool := flag.String("tool", "", "Provider tool ID; omit to list tools")
	input := flag.String("args-file", "-", "JSON argument file, or - for stdin")
	flag.Parse()
	address, ok := strings.CutPrefix(*ref, "grpc://")
	if !ok {
		return errors.New("connection must use grpc://")
	}
	if path, ok := strings.CutPrefix(address, "unix:"); ok {
		if !filepath.IsAbs(path) {
			return errors.New("Unix connection path must be absolute")
		}
		address = "unix:" + path
	} else {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return errors.New("TCP connection must be loopback")
		}
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	method, suffix := "list_tools", "ListTools"
	var payload proto.Message = &wire.ListToolsRequest{BindingId: binding}
	if *tool != "" {
		var reader io.Reader = os.Stdin
		if *input != "-" {
			f, err := os.Open(*input)
			if err != nil {
				return err
			}
			defer f.Close()
			reader = f
		}
		raw, err := io.ReadAll(io.LimitReader(reader, 2_000_001))
		if err != nil {
			return err
		}
		if len(raw) > 2_000_000 || !json.Valid(raw) {
			return errors.New("arguments must be bounded JSON")
		}
		method, suffix = "call_tool", "CallTool"
		payload = &wire.CallToolRequest{BindingId: *binding, ProviderToolId: *tool, ArgumentsJson: string(raw)}
	}
	raw, err := proto.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	response, err := wire.NewDirectPluginRuntimeClient(conn).Invoke(ctx, &wire.DirectInvocationRequest{Capability: "tool.provider.v1", InterfaceVersion: "1", Method: method, PayloadTypeUrl: "type.cyrene.io/cyrene.tool.provider.v1." + suffix + "Request", Payload: raw, RequestId: fmt.Sprintf("pilotctl-%d", time.Now().UnixNano())})
	if err != nil {
		return err
	}
	if e := response.GetError(); e != nil {
		return errors.New(e.Message)
	}
	if response.GetPayload().GetTypeUrl() != "type.cyrene.io/cyrene.tool.provider.v1."+suffix+"Response" {
		return errors.New("response type mismatch")
	}
	if *tool == "" {
		var result wire.ListToolsResponse
		if err := proto.Unmarshal(response.GetPayload().GetValue(), &result); err != nil {
			return err
		}
		if result.GetError() != nil {
			return errors.New(result.GetError().Message)
		}
		out, err := protojson.MarshalOptions{Indent: "  "}.Marshal(&result)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	var result wire.CallToolResponse
	if err := proto.Unmarshal(response.GetPayload().GetValue(), &result); err != nil {
		return err
	}
	if result.GetError() != nil {
		return errors.New(result.GetError().Message)
	}
	for _, part := range result.GetOutcome().GetContent() {
		if v := part.GetJson(); v != nil {
			fmt.Println(v.Json)
		} else if v := part.GetText(); v != nil {
			fmt.Println(v.Text)
		}
	}
	if result.GetOutcome().GetIsError() {
		return errors.New("workflow reported failure")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
