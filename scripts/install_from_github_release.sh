#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Install https-proxy (gateway + agent) from a GitHub Release.

Usage:
  install_from_github_release.sh --repo <owner/name> [--version <vX.Y.Z|latest>] [--install-dir <dir>]

Examples:
  sudo ./scripts/install_from_github_release.sh --repo yourorg/https-proxy --version latest
  sudo ./scripts/install_from_github_release.sh --repo yourorg/https-proxy --version v0.1.0 --install-dir /opt/https-proxy/bin

Notes:
  - Installs linux/amd64 artifacts:
      - https-proxy_gateway_<tag>_linux_amd64
      - https-proxy_agent_<tag>_linux_amd64
  - Verifies sha256 if https-proxy_<tag>_SHA256SUMS exists in the release.
EOF
}

REPO=""
VERSION="latest"
INSTALL_DIR="/opt/https-proxy/bin"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      REPO="${2:-}"; shift 2;;
    --version)
      VERSION="${2:-}"; shift 2;;
    --install-dir)
      INSTALL_DIR="${2:-}"; shift 2;;
    -h|--help)
      usage; exit 0;;
    *)
      echo "[Error] unknown arg: $1" >&2
      usage
      exit 2;;
  esac
done

if [[ -z "${REPO}" ]]; then
  echo "[Error] missing --repo <owner/name>" >&2
  exit 2
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "[Error] must run as root (installs into ${INSTALL_DIR})." >&2
  exit 2
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "[Error] curl not found." >&2
  exit 1
fi
if ! command -v tar >/dev/null 2>&1; then
  echo "[Error] tar not found." >&2
  exit 1
fi
if ! command -v sha256sum >/dev/null 2>&1; then
  echo "[Error] sha256sum not found." >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "[Error] python3 not found." >&2
  exit 1
fi

api_base="https://api.github.com/repos/${REPO}/releases"
json=""

if [[ "${VERSION}" == "latest" ]]; then
  json="$(curl -fsSL "${api_base}/latest")"
else
  # Find release by tag name.
  json="$(curl -fsSL "${api_base}/tags/${VERSION}")"
fi

tag="$(
  python3 - <<'PY'
import json, sys
data=json.loads(sys.stdin.read())
print(data.get("tag_name",""))
PY
  <<<"${json}"
)"

if [[ -z "${tag}" ]]; then
  echo "[Error] failed to resolve release tag (repo=${REPO} version=${VERSION})." >&2
  exit 1
fi

asset_gateway="https-proxy_gateway_${tag}_linux_amd64"
asset_agent="https-proxy_agent_${tag}_linux_amd64"
asset_sha="https-proxy_${tag}_SHA256SUMS"

gateway_url="$(
  python3 - <<'PY'
import json, sys
data=json.loads(sys.stdin.read())
name=sys.argv[1]
for a in data.get("assets",[]):
  if a.get("name")==name:
    print(a.get("browser_download_url",""))
    raise SystemExit(0)
raise SystemExit(1)
PY
  "${asset_gateway}" <<<"${json}" 2>/dev/null || true
)"

agent_url="$(
  python3 - <<'PY'
import json, sys
data=json.loads(sys.stdin.read())
name=sys.argv[1]
for a in data.get("assets",[]):
  if a.get("name")==name:
    print(a.get("browser_download_url",""))
    raise SystemExit(0)
raise SystemExit(1)
PY
  "${asset_agent}" <<<"${json}" 2>/dev/null || true
)"

sha_url="$(
  python3 - <<'PY'
import json, sys
data=json.loads(sys.stdin.read())
name=sys.argv[1]
for a in data.get("assets",[]):
  if a.get("name")==name:
    print(a.get("browser_download_url",""))
    raise SystemExit(0)
print("")
PY
  "${asset_sha}" <<<"${json}"
)"

if [[ -z "${gateway_url}" ]]; then
  echo "[Error] release asset not found: ${asset_gateway}" >&2
  exit 1
fi
if [[ -z "${agent_url}" ]]; then
  echo "[Error] release asset not found: ${asset_agent}" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

sha_path="${tmp_dir}/${asset_sha}"
gateway_path="${tmp_dir}/${asset_gateway}"
agent_path="${tmp_dir}/${asset_agent}"

echo "[Info] downloading ${gateway_url}"
curl -fsSL -o "${gateway_path}" "${gateway_url}"
echo "[Info] downloading ${agent_url}"
curl -fsSL -o "${agent_path}" "${agent_url}"

if [[ -n "${sha_url}" ]]; then
  echo "[Info] downloading ${sha_url}"
  curl -fsSL -o "${sha_path}" "${sha_url}"
  (
    cd "${tmp_dir}"
    sha256sum -c "${asset_sha}" --ignore-missing
  )
else
  echo "[Warn] sha256 asset not found; skipping integrity check."
fi

install -d -m 0755 "${INSTALL_DIR}"
install -m 0755 "${gateway_path}" "${INSTALL_DIR}/gateway"
install -m 0755 "${agent_path}" "${INSTALL_DIR}/agent"

echo "[OK] installed:"
echo "  ${INSTALL_DIR}/gateway (${tag})"
echo "  ${INSTALL_DIR}/agent (${tag})"
