#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
set -eu
cd "$(dirname "$0")/.."
module=github.com/danqing0b/cyrene-pilot-plugin
runtime=cyrene/plugin/runtime/v1/direct_plugin_runtime.proto
tool=cyrene/tool/provider/v1/tool_provider.proto
"${PROTOC:-protoc}" -I contracts/proto \
    --go_out=. --go_opt="module=$module" \
    --go_opt="M$runtime=$module/internal/wire" --go_opt="M$tool=$module/internal/wire" \
    --go-grpc_out=. --go-grpc_opt="module=$module" \
    --go-grpc_opt="M$runtime=$module/internal/wire" --go-grpc_opt="M$tool=$module/internal/wire" \
    "$runtime" "$tool"
