#!/usr/bin/env bash
set -euo pipefail

ROOT="/Users/zhangqiwei/Desktop/github/my-html-project/frp-0.48.0frps"
GO20="/opt/homebrew/opt/go@1.20/bin/go"
BRANCH="main"
REMOTE="origin"
REMOTE_URL="https://github.com/kiwi0712/taiwanfrpserver_client.git"
REPO="kiwi0712/taiwanfrpserver_client"

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

"$GO20" mod tidy

rm -rf "$ROOT/release"
mkdir -p "$ROOT/release"

# Build frps only (multi-platform)
os_archs=(
  "darwin:amd64"
  "darwin:arm64"
  "freebsd:386"
  "freebsd:amd64"
  "linux:386"
  "linux:amd64"
  "linux:arm"
  "linux:arm64"
  "windows:386"
  "windows:amd64"
  "windows:arm64"
  "linux:mips64"
  "linux:mips64le"
  "linux:mips:softfloat"
  "linux:mipsle:softfloat"
  "linux:riscv64"
)

for target in "${os_archs[@]}"; do
  os="$(echo "$target" | cut -d: -f1)"
  arch="$(echo "$target" | cut -d: -f2)"
  gomips="$(echo "$target" | cut -d: -f3)"
  suffix="${os}_${arch}"
  out="./release/frps_${suffix}"
  if [ "$os" = "windows" ]; then
    out="${out}.exe"
  fi
  echo "Build frps ${os}-${arch}..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOMIPS="$gomips" \
    PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" \
    "$GO20" build -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
done

# Build Android targets
unset CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS LDFLAGS
ANDROID_SKIPPED=()
for android_arch in arm64 arm amd64; do
  out="./release/frps_android_${android_arch}"
  echo "Build frps android-${android_arch}..."
  if [ "$android_arch" = "arm64" ]; then
    CGO_ENABLED=0 GOOS=android GOARCH="$android_arch" \
      PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" \
      "$GO20" build -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
    continue
  fi

  if [ "$android_arch" = "arm" ]; then
    NDK_CC="$(find_ndk_clang armv7a-linux-androideabi || true)"
    if [ -z "$NDK_CC" ]; then
      echo "Skip frps android-arm: missing armv7a-linux-androideabi${ANDROID_API_LEVEL}-clang"
      ANDROID_SKIPPED+=("android-arm")
      continue
    fi
    CGO_ENABLED=1 GOOS=android GOARCH=arm GOARM=7 CC="$NDK_CC" \
      PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" \
      "$GO20" build -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
  elif [ "$android_arch" = "amd64" ]; then
    NDK_CC="$(find_ndk_clang x86_64-linux-android || true)"
    if [ -z "$NDK_CC" ]; then
      echo "Skip frps android-amd64: missing x86_64-linux-android${ANDROID_API_LEVEL}-clang"
      ANDROID_SKIPPED+=("android-amd64")
      continue
    fi
    CGO_ENABLED=1 GOOS=android GOARCH=amd64 CC="$NDK_CC" \
      PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" \
      "$GO20" build -trimpath -ldflags "-s -w" -o "$out" ./cmd/frps
  fi
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

# Ensure remote points to target repository
if git remote get-url "$REMOTE" >/dev/null 2>&1; then
  git remote set-url "$REMOTE" "$REMOTE_URL"
else
  git remote add "$REMOTE" "$REMOTE_URL"
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
      --notes "taiwanfrp server $VERSION_TAG"
  else
    echo "gh CLI not found. Install with: brew install gh"
    echo "Then run: gh auth login"
  fi
fi

echo "Done. Release moved to Desktop."
