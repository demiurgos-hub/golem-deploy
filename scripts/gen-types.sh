#!/usr/bin/env sh
# Regenerate frontend/src/api/types.gen.ts from internal/apitypes (see tygo.yaml).
set -eu

TYGO_VERSION=v0.2.21

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
go run "github.com/gzuidhof/tygo@$TYGO_VERSION" generate
echo "Generated frontend/src/api/types.gen.ts"
