#!/usr/bin/env bash
# Builds the release files into dist/: the linux-amd64 binary with its SHA-256,
# install.sh and the systemd unit. The version comes from cmd/mpa-bridge/main.go.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
version="$(sed -n 's/^var Version = "\(.*\)"$/\1/p' cmd/mpa-bridge/main.go)"
[ -n "$version" ] || { echo "version not found in cmd/mpa-bridge/main.go" >&2; exit 1; }

rm -rf dist
mkdir -p dist
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=$version" \
	-o dist/mpa-bridge-linux-amd64 ./cmd/mpa-bridge
sum="$(sha256sum <dist/mpa-bridge-linux-amd64 | cut -d' ' -f1)"
echo "$sum  mpa-bridge-linux-amd64" >dist/mpa-bridge-linux-amd64.sha256
cp packaging/install.sh packaging/mpa-bridge.service dist/
echo "dist/ for mpa-bridge $version:"
ls -l dist
