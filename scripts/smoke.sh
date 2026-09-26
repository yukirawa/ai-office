#!/usr/bin/env sh
# ai-office Phase 0/1 スモークテスト。
#
# officed を起動し、worker を 2 体（dev_m / dev_f）接続させ、TUI の
# ヘッドレススナップショットと REST API で疎通を確認する。
# 成果物は server/data/smoke/ に出力する（.gitignore 済み）。
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${OFFICE_SMOKE_PORT:-18787}"
BASE="ws://127.0.0.1:${PORT}/ws"
HTTP="http://127.0.0.1:${PORT}"
OUT="${ROOT}/server/data/smoke"

mkdir -p "${OUT}"
rm -f "${OUT}"/*.db "${OUT}"/*.db-* "${OUT}"/*.log

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
OFFICE_LLM_PROVIDER="mock" \
OFFICE_PAYROLL_CRON="@every 5s" \
"${OUT}/officed" > "${OUT}/server.log" 2>&1 &
SRV_PID=$!
sleep 1

echo "== healthz =="
curl -fsS "${HTTP}/healthz"; echo

echo "== start workers (dev_m, dev_f) =="
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_m OFFICE_DEVICE_ID=zenbook \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_m.log" 2>&1 &
W1_PID=$!
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_f OFFICE_DEVICE_ID=zenbook \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_f.log" 2>&1 &
W2_PID=$!
sleep 2

echo "== employees (online になるはず) =="
curl -fsS "${HTTP}/api/employees"; echo

echo "== TUI snapshot (タスク投入前) =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --snapshot

echo "== POST /api/tasks (mgr に計画させる) =="
curl -fsS -X POST "${HTTP}/api/tasks" \
  -H 'Content-Type: application/json' \
  -d '{"title":"TUI の受け入れ確認","description":"Phase 0/1 の疎通確認","from":"owner"}'; echo
sleep 2

echo "== TUI snapshot (タスク投入後) =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --snapshot

echo "== ledger (mgr) =="
curl -fsS "${HTTP}/api/ledger/mgr"; echo

echo "== 日割り給与 cron を待つ（Phase 1.4, @every 5s） =="
sleep 6
echo "mgr : $(curl -fsS "${HTTP}/api/ledger/mgr")"
echo "dev_m: $(curl -fsS "${HTTP}/api/ledger/dev_m")"

echo "== TUI snapshot (給与支給後) =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --snapshot

echo "== stop workers (bye 送信 -> 退勤) =="
kill "${W1_PID}" 2>/dev/null || true
kill "${W2_PID}" 2>/dev/null || true
W1_PID=""
W2_PID=""
sleep 1

echo "== employees (退勤後) =="
curl -fsS "${HTTP}/api/employees"; echo

echo
echo "== server.log (tail) =="
tail -n 40 "${OUT}/server.log"

echo
echo "== dev_m.log (tail) =="
tail -n 20 "${OUT}/dev_m.log"

echo
echo "SMOKE OK"
