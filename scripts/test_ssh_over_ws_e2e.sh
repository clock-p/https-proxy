#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

need_cmd() {
  local cmd="$1"
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 1
  fi
}

pick_port() {
  python3 - <<'PY'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_for_http() {
  local url="$1"
  local timeout_sec="${2:-15}"
  local start
  start="$(date +%s)"
  while true; do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if (( "$(date +%s)" - start >= timeout_sec )); then
      return 1
    fi
    sleep 0.2
  done
}

wait_for_uuid_mode() {
  local status_url="$1"
  local uuid="$2"
  local mode="$3"
  local timeout_sec="${4:-20}"
  local start
  start="$(date +%s)"
  while true; do
    local status
    status="$(curl -fsS "${status_url}" || true)"
    if [[ -n "${status}" ]] && grep -q "\"${uuid}\":\"${mode}\"" <<<"${status}"; then
      return 0
    fi
    if (( "$(date +%s)" - start >= timeout_sec )); then
      echo "timeout waiting for uuid=${uuid} mode=${mode}" >&2
      echo "last status: ${status:-<empty>}" >&2
      return 1
    fi
    sleep 0.2
  done
}

run_ssh_cmd() {
  local key_file="$1"
  local proxy_cmd="$2"
  local remote_cmd="$3"
  ssh \
    -i "${key_file}" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o GlobalKnownHostsFile=/dev/null \
    -o IdentitiesOnly=yes \
    -o PreferredAuthentications=publickey \
    -o ConnectTimeout=15 \
    -o "ProxyCommand=${proxy_cmd}" \
    root@dummy "${remote_cmd}"
}

run_ssh_direct_port() {
  local key_file="$1"
  local port="$2"
  local remote_cmd="$3"
  ssh \
    -i "${key_file}" \
    -p "${port}" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o GlobalKnownHostsFile=/dev/null \
    -o IdentitiesOnly=yes \
    -o PreferredAuthentications=publickey \
    -o ConnectTimeout=15 \
    root@127.0.0.1 "${remote_cmd}"
}

need_cmd go
need_cmd ssh
need_cmd ssh-keygen
need_cmd sshd
need_cmd curl
need_cmd websocat
need_cmd wstunnel
need_cmd python3

TMP_DIR="$(mktemp -d)"
LOG_DIR="${TMP_DIR}/logs"
mkdir -p "${LOG_DIR}"
SCRIPT_OK=0

cleanup() {
  set +e
  for pid in "${CLI1_PID:-}" "${CLI2_PID:-}" "${GW_PID:-}" "${SSHD_PID:-}" "${WSTUNNEL_SERVER_PID:-}" "${WSTUNNEL_CLIENT_PID:-}"; do
    if [[ -n "${pid}" ]]; then
      kill "${pid}" >/dev/null 2>&1 || true
    fi
  done
  wait "${CLI1_PID:-}" >/dev/null 2>&1 || true
  wait "${CLI2_PID:-}" >/dev/null 2>&1 || true
  wait "${GW_PID:-}" >/dev/null 2>&1 || true
  wait "${SSHD_PID:-}" >/dev/null 2>&1 || true
  wait "${WSTUNNEL_SERVER_PID:-}" >/dev/null 2>&1 || true
  wait "${WSTUNNEL_CLIENT_PID:-}" >/dev/null 2>&1 || true
  if [[ "${SCRIPT_OK}" == "1" ]]; then
    rm -rf "${TMP_DIR}"
  else
    echo "debug logs kept at: ${TMP_DIR}" >&2
  fi
}
trap cleanup EXIT

SSHD_PORT="$(pick_port)"
GW_PORT="$(pick_port)"
WSTUNNEL_PORT="$(pick_port)"
WSTUNNEL_CLIENT_PORT="$(pick_port)"

SSH_HOST_KEY="${TMP_DIR}/ssh_host_ed25519_key"
SSH_CLIENT_KEY="${TMP_DIR}/ssh_client_ed25519_key"
SSH_AUTH_KEYS="${TMP_DIR}/authorized_keys"
SSH_CONFIG="${TMP_DIR}/sshd_config"

ssh-keygen -q -t ed25519 -N "" -f "${SSH_HOST_KEY}" >/dev/null
ssh-keygen -q -t ed25519 -N "" -f "${SSH_CLIENT_KEY}" >/dev/null
cp "${SSH_CLIENT_KEY}.pub" "${SSH_AUTH_KEYS}"

cat >"${SSH_CONFIG}" <<EOF
Port ${SSHD_PORT}
ListenAddress 127.0.0.1
HostKey ${SSH_HOST_KEY}
PidFile ${TMP_DIR}/sshd.pid
PasswordAuthentication no
ChallengeResponseAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthorizedKeysFile ${SSH_AUTH_KEYS}
PermitRootLogin yes
StrictModes no
UsePAM no
LogLevel VERBOSE
PrintMotd no
EOF

echo "[1/6] start temporary sshd on 127.0.0.1:${SSHD_PORT}"
/usr/sbin/sshd -D -f "${SSH_CONFIG}" -e >"${LOG_DIR}/sshd.log" 2>&1 &
SSHD_PID=$!
sleep 0.2

echo "[2/6] start gateway on 127.0.0.1:${GW_PORT}"
(
  cd "${REPO_ROOT}"
  HTTPS_PROXY_TRANSPARENT_ENABLED=true \
  HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS=u1,v1 \
  HTTPS_PROXY_FORCE_LEGACY_MODE=false \
  HTTPS_PROXY_AGENT_TOKENS="" \
  go run ./cmd/gateway --listen "127.0.0.1:${GW_PORT}"
) >"${LOG_DIR}/gateway.log" 2>&1 &
GW_PID=$!

if ! wait_for_http "http://127.0.0.1:${GW_PORT}/status" 20; then
  echo "gateway failed to start" >&2
  sed -n '1,200p' "${LOG_DIR}/gateway.log" >&2 || true
  exit 1
fi

echo "[3/6] test ssh + websocat (uuid=u1)"
(
  cd "${REPO_ROOT}"
  go run ./cmd/clockbridge-cli \
    --proxy-mode transparent_tunnel \
    -R "tcp://127.0.0.1:${SSHD_PORT}" \
    "u1@http://127.0.0.1:${GW_PORT}"
) >"${LOG_DIR}/cli-u1.log" 2>&1 &
CLI1_PID=$!

wait_for_uuid_mode "http://127.0.0.1:${GW_PORT}/status" "u1" "transparent_tunnel" 25

WS_RESULT="$(run_ssh_cmd "${SSH_CLIENT_KEY}" "websocat -b ws://127.0.0.1:${GW_PORT}/u1/ssh" "echo WS_OK" | tr -d '\r' | tail -n 1)"
if [[ "${WS_RESULT}" != "WS_OK" ]]; then
  echo "websocat path failed, got: ${WS_RESULT}" >&2
  sed -n '1,200p' "${LOG_DIR}/cli-u1.log" >&2 || true
  exit 1
fi
echo "websocat result: ${WS_RESULT}"

kill "${CLI1_PID}" >/dev/null 2>&1 || true
wait "${CLI1_PID}" >/dev/null 2>&1 || true
CLI1_PID=""

echo "[4/6] start wstunnel server"
wstunnel server --log-lvl DEBUG "ws://127.0.0.1:${WSTUNNEL_PORT}" >"${LOG_DIR}/wstunnel-server.log" 2>&1 &
WSTUNNEL_SERVER_PID=$!
sleep 0.3

echo "[5/6] test ssh + wstunnel (uuid=v1, local tcp endpoint)"
(
  cd "${REPO_ROOT}"
  go run ./cmd/clockbridge-cli \
    --proxy-mode transparent_tunnel \
    -R "ws://127.0.0.1:${WSTUNNEL_PORT}/v1" \
    "v1@http://127.0.0.1:${GW_PORT}"
) >"${LOG_DIR}/cli-u2.log" 2>&1 &
CLI2_PID=$!

wait_for_uuid_mode "http://127.0.0.1:${GW_PORT}/status" "v1" "transparent_tunnel" 25

wstunnel client \
  --log-lvl INFO \
  -L "tcp://127.0.0.1:${WSTUNNEL_CLIENT_PORT}:127.0.0.1:${SSHD_PORT}" \
  "ws://127.0.0.1:${GW_PORT}" >"${LOG_DIR}/wstunnel-client.log" 2>&1 &
WSTUNNEL_CLIENT_PID=$!
sleep 0.4

WS_TUNNEL_RESULT="$(run_ssh_direct_port "${SSH_CLIENT_KEY}" "${WSTUNNEL_CLIENT_PORT}" "echo WSTUNNEL_OK" | tr -d '\r' | tail -n 1)"
if [[ "${WS_TUNNEL_RESULT}" != "WSTUNNEL_OK" ]]; then
  echo "wstunnel path failed, got: ${WS_TUNNEL_RESULT}" >&2
  sed -n '1,200p' "${LOG_DIR}/cli-u2.log" >&2 || true
  sed -n '1,200p' "${LOG_DIR}/wstunnel-server.log" >&2 || true
  sed -n '1,200p' "${LOG_DIR}/wstunnel-client.log" >&2 || true
  exit 1
fi
echo "wstunnel result: ${WS_TUNNEL_RESULT}"

echo "[6/6] done"
echo "PASS: ssh over websocat and ssh over wstunnel both succeeded."
SCRIPT_OK=1
