#!/usr/bin/env bash
set -euo pipefail

# frp-0.63.0frps needs Go >= 1.23 (uses cmp/slices/math/rand/v2).
# Prefer Homebrew's current go instead of pinned go1.20.
if [ -x /opt/homebrew/opt/go/bin/go ]; then
  export PATH="/opt/homebrew/opt/go/bin:$PATH"
fi

if ! command -v go >/dev/null 2>&1; then
  echo "go not found in PATH" >&2
  exit 1
fi

echo "go: $(command -v go)"
go version

go_ver="$(go env GOVERSION | sed 's/^go//')"
go_major="${go_ver%%.*}"
go_rest="${go_ver#*.}"
go_minor="${go_rest%%.*}"
if [ "$go_major" -lt 1 ] || { [ "$go_major" -eq 1 ] && [ "$go_minor" -lt 23 ]; }; then
  echo "Go 1.23+ required (current: $go_ver)" >&2
  echo "Please use /opt/homebrew/opt/go/bin/go or /usr/local/go/bin/go." >&2
  exit 1
fi

cd /Users/zhangqiwei/Desktop/github/my-html-project/frp-0.63.0frps

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOTOOLCHAIN=local \
go build -mod=mod -trimpath -ldflags "-s -w" -o bin/frps_darwin_arm64 ./cmd/frps

mv -f bin/frps_darwin_arm64 /Users/zhangqiwei/Desktop/frps_darwin_arm64
echo "built: /Users/zhangqiwei/Desktop/frps_darwin_arm64"
