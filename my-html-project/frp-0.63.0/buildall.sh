#!/usr/bin/env bash
set -euo pipefail

ROOT="/Users/zhangqiwei/Desktop/github/my-html-project/frp-0.63.0"
GO_BIN="${GO_BIN:-go}"
REMOTE="${REMOTE:-redbean}"
REMOTE_URL="${REMOTE_URL:-https://github.com/redbean0721/taiwanfrp-launcher.git}"
REPO="${REPO:-redbean0721/taiwanfrp-launcher}"
PUSH_FORCE="${PUSH_FORCE:-0}"

cd "$ROOT"

if ! GIT_TOP="$(git rev-parse --show-toplevel 2>/dev/null)"; then
  echo "Not inside a git repository: $ROOT"
  exit 1
fi

if [[ "$ROOT" == "$GIT_TOP" ]]; then
  ROOT_REL="."
elif [[ "$ROOT" == "$GIT_TOP/"* ]]; then
  ROOT_REL="${ROOT#$GIT_TOP/}"
else
  echo "ROOT ($ROOT) is outside git top-level ($GIT_TOP). Abort."
  exit 1
fi

BRANCH="${BRANCH:-feature/golang-rewrite}"

if ! git -C "$GIT_TOP" remote get-url "$REMOTE" >/dev/null 2>&1; then
  git -C "$GIT_TOP" remote add "$REMOTE" "$REMOTE_URL"
fi

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

ANDROID_API_LEVEL="${ANDROID_API_LEVEL:-21}"
ANDROID_ALLOW_SKIP="${ANDROID_ALLOW_SKIP:-0}"

find_ndk_clang() {
  local target_triple="$1"
  local clang_name="${target_triple}${ANDROID_API_LEVEL}-clang"
  local ndk_root prebuilt_dir cc_path version_dir
  local -a ndk_roots=()

  if command -v "$clang_name" >/dev/null 2>&1; then
    command -v "$clang_name"
    return 0
  fi

  if [ -n "${ANDROID_NDK_HOME:-}" ]; then
    ndk_roots+=("${ANDROID_NDK_HOME}")
  fi
  if [ -n "${ANDROID_NDK_ROOT:-}" ]; then
    ndk_roots+=("${ANDROID_NDK_ROOT}")
  fi
  if [ -d "$HOME/Library/Android/sdk/ndk" ]; then
    for version_dir in "$HOME/Library/Android/sdk/ndk"/*; do
      [ -d "$version_dir" ] || continue
      ndk_roots+=("$version_dir")
    done
  fi
  if [ -d "$HOME/Library/Android/sdk/ndk-bundle" ]; then
    ndk_roots+=("$HOME/Library/Android/sdk/ndk-bundle")
  fi
  if [ -d "/opt/homebrew/share/android-ndk" ]; then
    ndk_roots+=("/opt/homebrew/share/android-ndk")
  fi
  if [ -d "/usr/local/share/android-ndk" ]; then
    ndk_roots+=("/usr/local/share/android-ndk")
  fi
  if [ -d "/opt/homebrew/Caskroom/android-ndk" ]; then
    for version_dir in /opt/homebrew/Caskroom/android-ndk/*; do
      [ -d "$version_dir" ] || continue
      for ndk_root in "$version_dir"/*; do
        [ -d "$ndk_root" ] || continue
        ndk_roots+=("$ndk_root")
      done
    done
  fi

  for ndk_root in "${ndk_roots[@]}"; do
    if [ ! -d "$ndk_root/toolchains/llvm/prebuilt" ]; then
      continue
    fi
    prebuilt_dir="$(find "$ndk_root/toolchains/llvm/prebuilt" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
    if [ -z "$prebuilt_dir" ]; then
      continue
    fi
    cc_path="$prebuilt_dir/bin/$clang_name"
    if [ -x "$cc_path" ]; then
      echo "$cc_path"
      return 0
    fi
  done

  return 1
}

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

"$GO_BIN" mod tidy

# Buildall always produces nogui artifacts for maximum cross-platform compatibility.
make -f Makefile.cross-compiles FRPC_TAGS="frpc nogui"

# Android targets (CLI/nogui only)
mkdir -p "$ROOT/release"
# Avoid shell-exported flags affecting Android external linker flow.
unset CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS LDFLAGS
ANDROID_SKIPPED=()
for ANDROID_ARCH in arm64 arm amd64; do
  ANDROID_OUT="$ROOT/release/taiwanfrp_android_${ANDROID_ARCH}"
  echo "Build android-${ANDROID_ARCH} (nogui)..."
  if [ "$ANDROID_ARCH" = "arm64" ]; then
    env CGO_ENABLED=0 GOOS=android GOARCH="$ANDROID_ARCH" \
      "$GO_BIN" build -trimpath -ldflags "-s -w" -tags "nogui" -o "$ANDROID_OUT" ./cmd/frpc
    continue
  fi

  # On Go 1.20, android/arm and android/amd64 require external linking.
  # Build them only when Android NDK clang is available.
  if [ "$ANDROID_ARCH" = "arm" ]; then
    NDK_CC="$(find_ndk_clang armv7a-linux-androideabi || true)"
    if [ -z "$NDK_CC" ]; then
      echo "Skip android-arm: missing armv7a-linux-androideabi${ANDROID_API_LEVEL}-clang"
      ANDROID_SKIPPED+=("android-arm")
      continue
    fi
    env CGO_ENABLED=1 GOOS=android GOARCH=arm GOARM=7 CC="$NDK_CC" \
      "$GO_BIN" build -trimpath -ldflags "-s -w" -tags "nogui" -o "$ANDROID_OUT" ./cmd/frpc
  elif [ "$ANDROID_ARCH" = "amd64" ]; then
    NDK_CC="$(find_ndk_clang x86_64-linux-android || true)"
    if [ -z "$NDK_CC" ]; then
      echo "Skip android-amd64: missing x86_64-linux-android${ANDROID_API_LEVEL}-clang"
      ANDROID_SKIPPED+=("android-amd64")
      continue
    fi
    env CGO_ENABLED=1 GOOS=android GOARCH=amd64 CC="$NDK_CC" \
      "$GO_BIN" build -trimpath -ldflags "-s -w" -tags "nogui" -o "$ANDROID_OUT" ./cmd/frpc
  fi
done

if [ "${#ANDROID_SKIPPED[@]}" -gt 0 ]; then
  echo "Android targets not built: ${ANDROID_SKIPPED[*]}"
  echo "Install Android NDK and export ANDROID_NDK_HOME, or add NDK clang to PATH."
  echo "If you want to continue without these targets, run: ANDROID_ALLOW_SKIP=1 ./buildall.sh"
  if [ "$ANDROID_ALLOW_SKIP" != "1" ]; then
    echo "Abort before commit: Android artifacts are incomplete."
    exit 1
  fi
fi

if [ -d "$ROOT/release" ]; then
  for artifact in "$ROOT"/release/*; do
    [ -f "$artifact" ] || continue
    add_version_suffix "$artifact"
  done
fi

git -C "$GIT_TOP" add -A -- "$ROOT_REL"

if git -C "$GIT_TOP" diff --cached --quiet -- "$ROOT_REL"; then
  echo "No tracked file changes under $ROOT_REL. Skip commit."
else
  read -r -p "Commit message: " MSG
  if [ -z "${MSG}" ]; then
    echo "Commit message cannot be empty. Abort."
    exit 1
  fi
  git -C "$GIT_TOP" -c status.showUntrackedFiles=no commit -m "$MSG"
fi

push_branch() {
  if [ "$PUSH_FORCE" = "1" ]; then
    git -C "$GIT_TOP" push -f "$REMOTE" "HEAD:$BRANCH"
  else
    git -C "$GIT_TOP" push "$REMOTE" "HEAD:$BRANCH"
  fi
}

if ! push_branch; then
  echo "Push failed once. Retry with HTTP/1.1..."
  git -C "$GIT_TOP" config http.version HTTP/1.1
  push_branch
fi

DESKTOP_RELEASE="/Users/zhangqiwei/Desktop/release"
if [ -d "$DESKTOP_RELEASE" ]; then
  TS=$(date +"%Y%m%d_%H%M%S")
  mv "$DESKTOP_RELEASE" "/Users/zhangqiwei/Desktop/release_$TS"
fi
mv "$ROOT/release" "/Users/zhangqiwei/Desktop/"

read -r -p "Create/update release tag ${VERSION_TAG}? [y/N]: " CREATE_TAG
if [[ "${CREATE_TAG}" =~ ^[Yy]$ ]]; then
  git -C "$GIT_TOP" tag -f "$VERSION_TAG"
  if ! git -C "$GIT_TOP" push -f "$REMOTE" "$VERSION_TAG"; then
    echo "Tag push failed once. Retry with HTTP/1.1..."
    git -C "$GIT_TOP" config http.version HTTP/1.1
    git -C "$GIT_TOP" push -f "$REMOTE" "$VERSION_TAG"
  fi
  if command -v gh >/dev/null 2>&1; then
    if gh auth status >/dev/null 2>&1; then
      if gh release view "$VERSION_TAG" --repo "$REPO" >/dev/null 2>&1; then
        gh release upload "$VERSION_TAG" /Users/zhangqiwei/Desktop/release/* \
          --repo "$REPO" \
          --clobber
      else
        gh release create "$VERSION_TAG" /Users/zhangqiwei/Desktop/release/* \
          --repo "$REPO" \
          --title "$VERSION_TAG" \
          --notes "taiwanfrp client $VERSION_TAG"
      fi
    else
      echo "gh CLI installed but not authenticated."
      echo "Run: gh auth login -h github.com"
      echo "Then rerun release upload/create."
    fi
  else
    echo "gh CLI not found. Install with: brew install gh"
    echo "Then run: gh auth login"
  fi
fi

echo "Done. Release moved to Desktop."
