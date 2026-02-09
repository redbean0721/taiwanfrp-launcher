#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRANCH="${BRANCH:-main}"
REMOTE="${REMOTE:-origin}"
REMOTE_URL="${REMOTE_URL:-https://github.com/kiwi0712/taiwanfrpserver_client.git}"
REPO="${REPO:-kiwi0712/taiwanfrpserver_client}"
DESKTOP_DIR="${DESKTOP_DIR:-$HOME/Desktop}"
RUN_TIDY="${RUN_TIDY:-1}"
PUSH_RETRY="${PUSH_RETRY:-5}"
PUSH_RETRY_DELAY="${PUSH_RETRY_DELAY:-3}"

if [ -n "${GO_BIN:-}" ] && [ -x "${GO_BIN}" ]; then
  :
elif [ -x /opt/homebrew/opt/go/bin/go ]; then
  GO_BIN="/opt/homebrew/opt/go/bin/go"
elif command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
else
  echo "go not found. Install Go >= 1.23 or set GO_BIN." >&2
  exit 1
fi

cd "$ROOT"

echo "go: $GO_BIN"
"$GO_BIN" version
go_ver="$("$GO_BIN" env GOVERSION | sed 's/^go//')"
go_major="${go_ver%%.*}"
go_rest="${go_ver#*.}"
go_minor="${go_rest%%.*}"
if [ "$go_major" -lt 1 ] || { [ "$go_major" -eq 1 ] && [ "$go_minor" -lt 23 ]; }; then
  echo "Go 1.23+ required (current: $go_ver)" >&2
  exit 1
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

if [ "$RUN_TIDY" = "1" ]; then
  "$GO_BIN" mod tidy -compat=1.23
fi

rm -rf "$ROOT/release"
mkdir -p "$ROOT/release"

# Build taiwanfrp (frps) for website targets
build_targets=(
  "darwin_amd64|darwin|amd64||"
  "darwin_arm64|darwin|arm64||"
  "freebsd_386|freebsd|386||"
  "freebsd_amd64|freebsd|amd64||"
  "linux_386|linux|386||"
  "linux_amd64|linux|amd64||"
  "linux_arm|linux|arm|5|"
  "linux_arm64|linux|arm64||"
  "linux_arm_hf|linux|arm|7|"
  "linux_loong64|linux|loong64||"
  "linux_mips|linux|mips||softfloat"
  "linux_mips64|linux|mips64||"
  "linux_mips64le|linux|mips64le||"
  "linux_mipsle|linux|mipsle||softfloat"
  "linux_riscv64|linux|riscv64||"
  "openbsd_386|openbsd|386||"
  "openbsd_amd64|openbsd|amd64||"
  "windows_386|windows|386||"
  "windows_amd64|windows|amd64||"
  "windows_arm64|windows|arm64||"
)

for target in "${build_targets[@]}"; do
  IFS='|' read -r label os arch goarm gomips <<< "$target"
  out="./release/taiwanfrp_${label}"
  if [ "$os" = "windows" ]; then
    out="${out}.exe"
  fi

  envs=(CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOTOOLCHAIN=local)
  if [ -n "$goarm" ]; then
    envs+=(GOARM="$goarm")
  fi
  if [ -n "$gomips" ]; then
    envs+=(GOMIPS="$gomips")
  fi

  echo "Build taiwanfrp ${label}..."
  env "${envs[@]}" "$GO_BIN" build -mod=mod -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
done

# Build Android targets
unset CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS LDFLAGS
ANDROID_SKIPPED=()
android_targets=(
  "android_arm64|arm64||0|"
  "android_arm|arm|7|1|armv7a-linux-androideabi"
  "android_amd64|amd64||1|x86_64-linux-android"
  "android_386|386||1|i686-linux-android"
)
for target in "${android_targets[@]}"; do
  IFS='|' read -r label arch goarm need_cgo ndk_triple <<< "$target"
  out="./release/taiwanfrp_${label}"
  echo "Build taiwanfrp ${label}..."

  if [ "$need_cgo" = "1" ]; then
    NDK_CC="$(find_ndk_clang "$ndk_triple" || true)"
    if [ -z "$NDK_CC" ]; then
      echo "Skip ${label}: missing ${ndk_triple}${ANDROID_API_LEVEL}-clang"
      ANDROID_SKIPPED+=("${label}")
      continue
    fi
    envs=(CGO_ENABLED=1 GOOS=android GOARCH="$arch" CC="$NDK_CC" GOTOOLCHAIN=local)
  else
    envs=(CGO_ENABLED=0 GOOS=android GOARCH="$arch" GOTOOLCHAIN=local)
  fi
  if [ -n "$goarm" ]; then
    envs+=(GOARM="$goarm")
  fi
  env "${envs[@]}" "$GO_BIN" build -mod=mod -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
done

if [ "${#ANDROID_SKIPPED[@]}" -gt 0 ]; then
  echo "Android targets not built: ${ANDROID_SKIPPED[*]}"
  echo "Install Android NDK and export ANDROID_NDK_HOME, or add NDK clang to PATH."
  echo "If you want to continue without these targets, run: ANDROID_ALLOW_SKIP=1 ./push_release.sh"
  if [ "$ANDROID_ALLOW_SKIP" != "1" ]; then
    echo "Abort before commit: Android artifacts are incomplete."
    exit 1
  fi
fi

# Add version suffix to artifacts
if [ -d "$ROOT/release" ]; then
  for artifact in "$ROOT"/release/*; do
    [ -f "$artifact" ] || continue
    add_version_suffix "$artifact"
  done
fi

# Verify website-required artifact list
required_artifacts=(
  "taiwanfrp_android_386_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_android_amd64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_android_arm64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_android_arm_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_darwin_amd64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_darwin_arm64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_freebsd_386_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_freebsd_amd64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_386_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_amd64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_arm_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_arm64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_arm_hf_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_loong64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_mips_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_mips64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_mips64le_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_mipsle_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_linux_riscv64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_openbsd_386_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_openbsd_amd64_${VERSION_FILE_SUFFIX}"
  "taiwanfrp_windows_386_${VERSION_FILE_SUFFIX}.exe"
  "taiwanfrp_windows_amd64_${VERSION_FILE_SUFFIX}.exe"
  "taiwanfrp_windows_arm64_${VERSION_FILE_SUFFIX}.exe"
)

missing_artifacts=()
for artifact in "${required_artifacts[@]}"; do
  if [ ! -f "$ROOT/release/$artifact" ]; then
    missing_artifacts+=("$artifact")
  fi
done

if [ "${#missing_artifacts[@]}" -gt 0 ]; then
  echo "Missing required artifacts for website:"
  printf '  %s\n' "${missing_artifacts[@]}"
  if [ "$ANDROID_ALLOW_SKIP" = "1" ]; then
    echo "Continue because ANDROID_ALLOW_SKIP=1"
  else
    exit 1
  fi
fi

# Ensure remote points to target repository
if git remote get-url "$REMOTE" >/dev/null 2>&1; then
  git remote set-url "$REMOTE" "$REMOTE_URL"
else
  git remote add "$REMOTE" "$REMOTE_URL"
fi

push_with_retry() {
  local target_desc="$1"
  shift

  local attempt=1
  while [ "$attempt" -le "$PUSH_RETRY" ]; do
    if git -c http.version=HTTP/1.1 -c http.postBuffer=524288000 push "$@"; then
      return 0
    fi
    if [ "$attempt" -lt "$PUSH_RETRY" ]; then
      local sleep_secs=$((PUSH_RETRY_DELAY * attempt))
      echo "Push ${target_desc} failed (attempt ${attempt}/${PUSH_RETRY}), retry in ${sleep_secs}s..."
      sleep "$sleep_secs"
    fi
    attempt=$((attempt + 1))
  done

  echo "Push ${target_desc} failed after ${PUSH_RETRY} attempts."
  return 1
}

git add .
read -r -p "Commit message: " MSG
if [ -z "${MSG}" ]; then
  echo "Commit message cannot be empty. Abort."
  exit 1
fi
git commit -m "$MSG" || true

push_with_retry "branch ${BRANCH}" -f "$REMOTE" HEAD:"$BRANCH"

mkdir -p "$DESKTOP_DIR"
DESKTOP_RELEASE="${DESKTOP_DIR}/release"
if [ -d "$DESKTOP_RELEASE" ]; then
  TS=$(date +"%Y%m%d_%H%M%S")
  mv "$DESKTOP_RELEASE" "${DESKTOP_DIR}/release_$TS"
fi
mv "$ROOT/release" "$DESKTOP_DIR/"

read -r -p "Create/update release tag ${VERSION_TAG}? [y/N]: " CREATE_TAG
if [[ "${CREATE_TAG}" =~ ^[Yy]$ ]]; then
  git tag -f "$VERSION_TAG"
  push_with_retry "tag ${VERSION_TAG}" -f "$REMOTE" "$VERSION_TAG"
  if command -v gh >/dev/null 2>&1; then
    gh release create "$VERSION_TAG" "${DESKTOP_DIR}"/release/* \
      --repo "$REPO" \
      --title "$VERSION_TAG" \
      --notes "taiwanfrp server $VERSION_TAG"
  else
    echo "gh CLI not found. Install with: brew install gh"
    echo "Then run: gh auth login"
  fi
fi

echo "Done. Release moved to ${DESKTOP_DIR}."
