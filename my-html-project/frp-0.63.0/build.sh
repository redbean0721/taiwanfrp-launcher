#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_BIN="${GO_BIN:-go}"
OUTPUT_NAME="${OUTPUT_NAME:-taiwanfrpclient}"
DEST_DIR="${DEST_DIR:-$(cd "${ROOT_DIR}/../../.." && pwd)}"
BUILD_TAGS="${BUILD_TAGS:-}"
AUTO_TIDY="${AUTO_TIDY:-1}"

if ! command -v "${GO_BIN}" >/dev/null 2>&1; then
  echo "找不到 Go 編譯器: ${GO_BIN}"
  exit 1
fi

GO_VERSION_RAW="$("${GO_BIN}" version | awk '{print $3}')"
GO_VERSION="${GO_VERSION_RAW#go}"
GO_MAJOR="${GO_VERSION%%.*}"
GO_REST="${GO_VERSION#*.}"
GO_MINOR="${GO_REST%%.*}"

if [ "${GO_MAJOR}" -lt 1 ] || [ "${GO_MINOR}" -lt 23 ]; then
  echo "目前 Go 版本: ${GO_VERSION_RAW}"
  echo "frp-0.63.0 需要 Go >= 1.23（程式碼使用了 cmp/slices/min/max）。"
  echo "請改用較新版本，例如："
  echo "  GO_BIN=go ./build.sh"
  echo "或安裝 go@1.23+ 後再編譯。"
  exit 1
fi

cd "${ROOT_DIR}"

build_cmd=("${GO_BIN}" build -o "${OUTPUT_NAME}" ./cmd/frpc)
if [ -n "${BUILD_TAGS}" ]; then
  build_cmd=("${GO_BIN}" build -tags "${BUILD_TAGS}" -o "${OUTPUT_NAME}" ./cmd/frpc)
fi

BUILD_ERR_FILE="$(mktemp -t taiwanfrp_build_err.XXXXXX)"
if ! "${build_cmd[@]}" 2>"${BUILD_ERR_FILE}"; then
  if [ "${AUTO_TIDY}" = "1" ] && grep -q "updates to go.mod needed" "${BUILD_ERR_FILE}"; then
    echo "偵測到 go.mod 需要更新，正在執行: ${GO_BIN} mod tidy"
    "${GO_BIN}" mod tidy
    if ! "${build_cmd[@]}" 2>"${BUILD_ERR_FILE}"; then
      cat "${BUILD_ERR_FILE}" >&2
      rm -f "${BUILD_ERR_FILE}"
      exit 1
    fi
  else
    cat "${BUILD_ERR_FILE}" >&2
    rm -f "${BUILD_ERR_FILE}"
    exit 1
  fi
fi
rm -f "${BUILD_ERR_FILE}"

mkdir -p "${DEST_DIR}"
mv -f "${OUTPUT_NAME}" "${DEST_DIR}/${OUTPUT_NAME}"
echo "完成: ${DEST_DIR}/${OUTPUT_NAME}"
