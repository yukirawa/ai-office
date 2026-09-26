#!/usr/bin/env sh
# ai-office Phase 0/1 対話デモ。
#
# officed を起動し、worker を 2 体（dev_m / dev_f）接続させたうえで、
# read-only TUI をフォアグラウンドで起動する。TUI を終了（q / Ctrl-C）すると
# worker と officed も停止する。
#
#   sh scripts/demo.sh            # 既定ポート 8787
#   OFFICE_PORT=9000 sh scripts/demo.sh
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${OFFICE_PORT:-8787}"
BASE="ws://127.0.0.1:${PORT}/ws"
HTTP="http://127.0.0.1:${PORT}"
OUT="${ROOT}/server/data/demo"

mkdir -p "${OUT}"

echo "== build =="
(cd "${ROOT}/server" && go build -o "${OUT}/officed" ./cmd/officed)
(cd "${ROOT}/client" && cargo build --workspace -q)

SRV_PID=""
W1_PID=""
W2_PID=""
cleanup() {
  for p in "${W1_PID}" "${W2_PID}" "${SRV_PID}"; do
    if [ -n "${p}" ]; then kill "${p}" 2>/dev/null || true; fi
  done
}
trap cleanup EXIT INT TERM

echo "== start officed on :${PORT} =="
OFFICE_DB="${OUT}/office.db" \
OFFICE_ADDR="127.0.0.1:${PORT}" \
"${OUT}/officed" > "${OUT}/server.log" 2>&1 &
SRV_PID=$!
sleep 1

echo "== start workers (dev_m, dev_f) =="
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_m OFFICE_DEVICE_ID=zenbook \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_m.log" 2>&1 &
W1_PID=$!
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_f OFFICE_DEVICE_ID=zenbook \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_f.log" 2>&1 &
W2_PID=$!
sleep 2

echo
echo "TUI を起動します。q または Ctrl-C で終了します。"
echo "別ターミナルからタスクを投入できます:"
echo "  curl -X POST ${HTTP}/api/tasks -H 'Content-Type: application/json' \\"
echo "    -d '{\"title\":\"ログイン画面の実装\",\"description\":\"...\",\"from\":\"owner\"}'"
echo

OFFICE_SERVER_URL="${BASE}" OFFICE_TUI_ID="${OFFICE_TUI_ID:-owner}" \
  "${ROOT}/client/target/debug/tui"

echo
echo "TUI を終了しました。ログ: ${OUT}/server.log"
