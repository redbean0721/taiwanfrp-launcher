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

"$GO20" mod tidy
export GOFLAGS="-tags=nogui"
PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" make -f Makefile.cross-compiles

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

read -r -p "Release tag (e.g. v1.0.0, empty to skip): " TAG
if [ -n "$TAG" ]; then
  git tag -f "$TAG"
  git push -f "$REMOTE" "$TAG"
  if command -v gh >/dev/null 2>&1; then
    gh release create "$TAG" /Users/zhangqiwei/Desktop/release/* \
      --repo "$REPO" \
      --title "$TAG" \
      --notes "taiwanfrp client $TAG"
  else
    echo "gh CLI not found. Install with: brew install gh"
    echo "Then run: gh auth login"
  fi
fi

echo "Done. Release moved to Desktop."
