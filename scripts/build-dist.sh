#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST="$ROOT/dist"
LINUX="$DIST/linux-amd64"
CACHE="$DIST/.gocache"
mkdir -p "$LINUX" "$CACHE"

echo "==> build linux/amd64 binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cfdata" "$ROOT/cfdata.go"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cfnat" "$ROOT/cfnat.go"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cloudflare-web" "$ROOT/web"

echo "==> copy CA bundle"
if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  cp /etc/ssl/certs/ca-certificates.crt "$DIST/ca-certificates.crt"
elif [ -f /etc/pki/tls/certs/ca-bundle.crt ]; then
  cp /etc/pki/tls/certs/ca-bundle.crt "$DIST/ca-certificates.crt"
else
  echo "No system CA bundle found. Put one at dist/ca-certificates.crt" >&2
  exit 1
fi

echo "==> dist ready"
ls -lh "$LINUX" "$DIST/ca-certificates.crt"
