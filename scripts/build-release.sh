#!/usr/bin/env bash
# Cross-compile platform-bridge-wecom for win/mac/linux × amd64/arm64.
# Output: dist/<version>/<name>-<os>-<arch>[.exe] + archives (zip for windows, tar.gz otherwise).
#
# Usage:
#   scripts/build-release.sh              # use VERSION file
#   VERSION=1.2.3 scripts/build-release.sh
#   scripts/build-release.sh linux/amd64  # single target
set -euo pipefail

cd "$(dirname "$0")/.."

BINARY_NAME="platform-bridge-wecom"
VERSION="${VERSION:-$(cat VERSION 2>/dev/null || echo 0.0.0-dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

DEFAULT_TARGETS=(
  "windows/amd64"
  "windows/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
)

if [ $# -gt 0 ]; then
  TARGETS=("$@")
else
  TARGETS=("${DEFAULT_TARGETS[@]}")
fi

OUT_ROOT="dist/${VERSION}"
rm -rf "${OUT_ROOT}"
mkdir -p "${OUT_ROOT}"

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

echo "==> building ${BINARY_NAME} ${VERSION} (${COMMIT})"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  suffix=""
  if [ "${GOOS}" = "windows" ]; then suffix=".exe"; fi

  pkg_name="${BINARY_NAME}-${VERSION}-${GOOS}-${GOARCH}"
  pkg_dir="${OUT_ROOT}/${pkg_name}"
  mkdir -p "${pkg_dir}"

  bin_path="${pkg_dir}/${BINARY_NAME}${suffix}"
  echo "--> ${GOOS}/${GOARCH} -> ${bin_path}"

  GOOS="${GOOS}" GOARCH="${GOARCH}" CGO_ENABLED=0 \
    go build -trimpath -ldflags="${LDFLAGS}" -o "${bin_path}" ./cmd/bridge

  cp README.md SPEC.md VERSION "${pkg_dir}/" 2>/dev/null || true
  if [ -f .env.example ]; then cp .env.example "${pkg_dir}/"; fi

  # Write manifest.json from template
  sed -e "s/__VERSION__/${VERSION}/g" \
      -e "s/__TARGET_OS__/${GOOS}/g" \
      -e "s/__TARGET_ARCH__/${GOARCH}/g" \
      -e "s/__BACKEND_ENTRY__/${BINARY_NAME}${suffix}/g" \
      -e "s/__START_SCRIPT__/start.sh/g" \
      -e "s/__STOP_SCRIPT__/stop.sh/g" \
      "${SCRIPT_DIR}/release-assets/manifest.template.json" > "${pkg_dir}/manifest.json"

  # Copy start/stop scripts
  cp "${SCRIPT_DIR}/release-assets/start.sh" "${pkg_dir}/"
  cp "${SCRIPT_DIR}/release-assets/stop.sh" "${pkg_dir}/"

  pushd "${OUT_ROOT}" > /dev/null
  if [ "${GOOS}" = "windows" ]; then
    if command -v zip > /dev/null; then
      zip -qr "${pkg_name}.zip" "${pkg_name}"
    elif command -v python3 > /dev/null; then
      python3 -c "import shutil,sys; shutil.make_archive(sys.argv[1],'zip',root_dir='.',base_dir=sys.argv[1])" "${pkg_name}"
    else
      echo "    (neither zip nor python3 found; skipping archive for ${pkg_name})"
    fi
  else
    tar -czf "${pkg_name}.tar.gz" "${pkg_name}"
  fi
  popd > /dev/null
done

echo ""
echo "==> artifacts in ${OUT_ROOT}:"
ls -la "${OUT_ROOT}" | awk '{print "    "$0}'

if command -v sha256sum > /dev/null; then
  (cd "${OUT_ROOT}" && sha256sum *.zip *.tar.gz 2>/dev/null > SHA256SUMS.txt || true)
  if [ -s "${OUT_ROOT}/SHA256SUMS.txt" ]; then
    echo ""
    echo "==> SHA256SUMS:"
    cat "${OUT_ROOT}/SHA256SUMS.txt" | awk '{print "    "$0}'
  fi
fi
