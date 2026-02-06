#!/usr/bin/env bash
set -euo pipefail

ROOT="/Users/zhangqiwei/Desktop/github/my-html-project/frp-0.48.0"
GO20="/opt/homebrew/opt/go@1.20/bin/go"
BRANCH="feature/golang-rewrite"
REMOTE="origin"

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
PATH="/opt/homebrew/opt/go@1.20/bin:$PATH" make -f Makefile.cross-compiles

git add .
read -r -p "Commit message: " MSG
if [ -z "${MSG}" ]; then
  echo "Commit message cannot be empty. Abort."
  exit 1
fi
git commit -m "$MSG" || true
git push -f "$REMOTE" HEAD:"$BRANCH"

mv "$ROOT/release" "/Users/zhangqiwei/Desktop/"

echo "Done. Release moved to Desktop."
