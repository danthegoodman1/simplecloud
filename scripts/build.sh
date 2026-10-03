#!/usr/bin/env sh
# Builds the agent for linux/amd64, embeds it, then builds the CLI.
#
# The agent must be amd64 because that is the only architecture sandboxes run, and
# it is built static (CGO off) so it needs nothing from the image.
set -eu
cd "$(dirname "$0")/.."

echo "building scagent for linux/amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath -ldflags "-s -w" \
  -o /tmp/scagent ./cmd/scagent
gzip -9 -c /tmp/scagent > internal/agentbin/scagent.gz
printf "  agent %s bytes, packed %s bytes\n" \
  "$(wc -c < /tmp/scagent | tr -d ' ')" \
  "$(wc -c < internal/agentbin/scagent.gz | tr -d ' ')"

echo "building simplecloud"
mkdir -p bin
go build -trimpath -o bin/simplecloud ./cmd/simplecloud
echo "  bin/simplecloud"
