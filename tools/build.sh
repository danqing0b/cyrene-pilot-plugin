#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
set -eu

usage() {
    printf '%s\n' 'Usage: tools/build.sh [--output DIRECTORY]' \
        'Runs Go vet and race tests, then builds the Cyrene ZIP and descriptor.' \
        'Defaults to dist/ under the plugin directory.' \
        'Set CYRENE_GO or CYRENE_PYTHON to select a toolchain executable.'
}

plugin_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
output_dir="$plugin_dir/dist"
case $# in
    0) ;;
    1)
        if [ "$1" = '--help' ] || [ "$1" = '-h' ]; then
            usage
            exit 0
        fi
        usage >&2
        exit 2
        ;;
    2)
        if [ "$1" != '--output' ] || [ -z "$2" ]; then
            usage >&2
            exit 2
        fi
        case $2 in
            /*) output_dir=$2 ;;
            *) output_dir="$PWD/$2" ;;
        esac
        ;;
    *) usage >&2; exit 2 ;;
esac

resolve_tool() {
    tool_path=$(command -v "$1") || {
        printf 'Executable not found: %s\n' "$1" >&2
        return 1
    }
    case $tool_path in
        /*) printf '%s\n' "$tool_path" ;;
        *) printf '%s/%s\n' "$PWD" "$tool_path" ;;
    esac
}

go_bin=$(resolve_tool "${CYRENE_GO:-go}")
python_bin=$(resolve_tool "${CYRENE_PYTHON:-python3}")
go_version=$("$go_bin" version 2>/dev/null) || go_version=''
case $go_version in
    'go version go'*) ;;
    *) printf 'Set CYRENE_GO to the Go compiler executable.\n' >&2; exit 1 ;;
esac

cd "$plugin_dir"
"$go_bin" vet -mod=readonly ./...
CGO_ENABLED=1 "$go_bin" test -mod=readonly -race ./...
"$python_bin" tools/package.py --output "$output_dir" --go "$go_bin"
CGO_ENABLED=0 "$go_bin" build -mod=readonly -trimpath -buildvcs=false -o "$output_dir/cyrene-pilot" .
CGO_ENABLED=0 "$go_bin" build -mod=readonly -trimpath -buildvcs=false -o "$output_dir/pilotctl" ./cmd/pilotctl
