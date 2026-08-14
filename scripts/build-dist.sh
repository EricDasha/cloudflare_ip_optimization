#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST="$ROOT/dist"
LINUX="$DIST/linux-amd64"
CACHE="$DIST/.gocache"
SING_BOX_VERSION="${SING_BOX_VERSION:-1.13.18}"
SING_BOX_VERSION="${SING_BOX_VERSION#v}"
SING_BOX_BUILD_ID="$SING_BOX_VERSION|with_utls|static-v1"
mkdir -p "$LINUX" "$CACHE"

echo "==> build linux/amd64 binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cfdata" "$ROOT/cfdata.go"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cfnat" "$ROOT/cfnat.go"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
  go build -trimpath -ldflags="-s -w" -o "$LINUX/cloudflare-web" "$ROOT/web"

INSTALLED_SING_BOX_VERSION="$(cat "$LINUX/sing-box.version" 2>/dev/null || true)"
if [ ! -f "$LINUX/sing-box" ] || [ "$INSTALLED_SING_BOX_VERSION" != "$SING_BOX_BUILD_ID" ]; then
  echo "==> build static sing-box v$SING_BOX_VERSION (with_utls)"
  (
    unset GOBIN
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTELEMETRY=off GOCACHE="$CACHE" \
      go install -trimpath \
        -ldflags="-s -w -X github.com/sagernet/sing-box/constant.Version=$SING_BOX_VERSION" \
        -tags with_utls "github.com/sagernet/sing-box/cmd/sing-box@v$SING_BOX_VERSION"
  )
  GOPATH_VALUE="$(go env GOPATH)"
  cp "$GOPATH_VALUE/bin/linux_amd64/sing-box" "$LINUX/sing-box"
  chmod 0755 "$LINUX/sing-box"
  printf '%s' "$SING_BOX_BUILD_ID" > "$LINUX/sing-box.version"
fi

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
