#!/usr/bin/env sh
# Build the web client, then the golem-deploy binary into bin/.
# Extra arguments are passed to `go build` (for example: -ldflags "-s -w").
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root/frontend"
npm ci
npm run build

cd "$root"
mkdir -p bin
go build "$@" -o bin/golem-deploy ./cmd/golem-deploy
echo "Built $root/bin/golem-deploy"
