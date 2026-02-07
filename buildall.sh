#!/usr/bin/env bash
set -euo pipefail

ROOT="/Users/zhangqiwei/Desktop/github/my-html-project/frp-0.48.0"
GO20="/opt/homebrew/opt/go@1.20/bin/go"
BRANCH="feature/golang-rewrite"
REMOTE="origin"
REPO="redbean0721/taiwanfrp-launcher"

cd "$ROOT"

# Clean macOS junk files
find "$ROOT" -name ".DS_Store" -delete

# Ensure .DS_Store ignored
if [ ! -f .gitignore ]; then
  touch .gitignore
fi
if ! grep -q "^\\.DS_Store$" .gitignore; then
  echo ".DS_Store" >> .gitignore
fi

read -r -p "Version (e.g. 1.0.0 or v1.0.0): " VERSION_INPUT
if [ -z "${VERSION_INPUT}" ]; then
  echo "Version cannot be empty. Abort."
  exit 1
fi

if [[ "$VERSION_INPUT" =~ ^v ]]; then
  VERSION_TAG="$VERSION_INPUT"
else
  VERSION_TAG="v${VERSION_INPUT}"
fi
VERSION_FILE_SUFFIX="$(printf "%s" "$VERSION_TAG" | tr -c 'A-Za-z0-9._-' '_')"
if [ -z "${VERSION_FILE_SUFFIX}" ]; then
  echo "Invalid version string. Abort."
  exit 1
fi

add_version_suffix() {
  local file="$1"
  local dir base stem ext new_name
  dir="$(dirname "$file")"
  base="$(basename "$file")"

  if [[ "$base" == *"_${VERSION_FILE_SUFFIX}" ]] || [[ "$base" == *"_${VERSION_FILE_SUFFIX}."* ]]; then
    return 0
  fi

  if [[ "$base" == *.* ]]; then
    stem="${base%.*}"
    ext=".${base##*.}"
  else
    stem="$base"
    ext=""
  fi

  new_name="${dir}/${stem}_${VERSION_FILE_SUFFIX}${ext}"
  mv "$file" "$new_name"
}

"$GO20" mod tidy

# Fyne GUI cannot be reliably cross-compiled for every OS/arch in one host env.
# Build all targets as nogui first, then overwrite host target with GUI build.
export GOFLAGS="-tags=nogui"
PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" make -f Makefile.cross-compiles

unset GOFLAGS
HOST_OS="$("$GO20" env GOOS)"
HOST_ARCH="$("$GO20" env GOARCH)"
HOST_OUT="$ROOT/release/taiwanfrp_${HOST_OS}_${HOST_ARCH}_${VERSION_FILE_SUFFIX}"
if [ "$HOST_OS" = "windows" ]; then
  HOST_OUT="${HOST_OUT}.exe"
fi
echo "Build host GUI binary: ${HOST_OS}-${HOST_ARCH}"
CGO_ENABLED=1 "$GO20" build -o "$HOST_OUT" ./cmd/frpc

if [ -d "$ROOT/release" ]; then
  for artifact in "$ROOT"/release/*; do
    [ -f "$artifact" ] || continue
    add_version_suffix "$artifact"
  done
fi

git add .
read -r -p "Commit message: " MSG
if [ -z "${MSG}" ]; then
  echo "Commit message cannot be empty. Abort."
  exit 1
fi
git commit -m "$MSG" || true
git push -f "$REMOTE" HEAD:"$BRANCH"

DESKTOP_RELEASE="/Users/zhangqiwei/Desktop/release"
if [ -d "$DESKTOP_RELEASE" ]; then
  TS=$(date +"%Y%m%d_%H%M%S")
  mv "$DESKTOP_RELEASE" "/Users/zhangqiwei/Desktop/release_$TS"
fi
mv "$ROOT/release" "/Users/zhangqiwei/Desktop/"

read -r -p "Create/update release tag ${VERSION_TAG}? [y/N]: " CREATE_TAG
if [[ "${CREATE_TAG}" =~ ^[Yy]$ ]]; then
  git tag -f "$VERSION_TAG"
  git push -f "$REMOTE" "$VERSION_TAG"
  if command -v gh >/dev/null 2>&1; then
    gh release create "$VERSION_TAG" /Users/zhangqiwei/Desktop/release/* \
      --repo "$REPO" \
      --title "$VERSION_TAG" \
      --notes "taiwanfrp client $VERSION_TAG"
  else
    echo "gh CLI not found. Install with: brew install gh"
    echo "Then run: gh auth login"
  fi
fi

echo "Done. Release moved to Desktop."
