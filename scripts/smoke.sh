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
rm -rf "${OUT}/ws"
rm -f "${OUT}"/*.db "${OUT}"/*.db-* "${OUT}"/*.log
mkdir -p "${OUT}/ws/dev_m" "${OUT}/ws/dev_f"

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
OFFICE_CHAT_CRON="@every 5s" \
OFFICE_INEQUALITY_THRESHOLD=1 \
"${OUT}/officed" > "${OUT}/server.log" 2>&1 &
SRV_PID=$!
sleep 1

echo "== healthz =="
curl -fsS "${HTTP}/healthz"; echo

echo "== start workers (dev_m, dev_f) =="
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_m OFFICE_DEVICE_ID=zenbook \
  OFFICE_WORKSPACE="${OUT}/ws/dev_m" OFFICE_SANDBOX="none" \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_m.log" 2>&1 &
W1_PID=$!
OFFICE_SERVER_URL="${BASE}" OFFICE_EMPLOYEE_ID=dev_f OFFICE_DEVICE_ID=zenbook \
  OFFICE_WORKSPACE="${OUT}/ws/dev_f" OFFICE_SANDBOX="none" \
  "${ROOT}/client/target/debug/worker" > "${OUT}/dev_f.log" 2>&1 &
W2_PID=$!
sleep 2

echo "== employees (online になるはず) =="
curl -fsS "${HTTP}/api/employees"; echo

echo "== TUI snapshot (タスク投入前) =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --snapshot

echo "== POST /api/tasks (mgr -> dev に割当て、worker が実行) =="
curl -fsS -X POST "${HTTP}/api/tasks" \
  -H 'Content-Type: application/json' \
  -d '{"title":"TUI の受け入れ確認","description":"Phase 0/1/2 の疎通確認","from":"owner"}'; echo
sleep 3

echo "== tasks (状態遷移: pending -> assigned -> working -> review -> done) =="
curl -fsS "${HTTP}/api/tasks?limit=3"; echo

echo "== worker が実際に書いたファイル（Phase 2.1 Local モード） =="
find "${OUT}/ws" -type f | sort
echo "-- report の中身 --"
find "${OUT}/ws" -name '*.md' -exec cat {} \;

echo "== POST /api/chat (Phase 4.3: chat 役が応答) =="
curl -fsS -X POST "${HTTP}/api/chat" -H 'Content-Type: application/json' \
  -d '{"message":"おはよう。今日の様子はどう？"}'; echo
sleep 2

echo "== TUI snapshot (Phase 4: タスク/雑談/関係値) =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --snapshot

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

# ---- Phase 5: 経済・社会（学の元帳・格差の観察） ----
# OFFICE_INEQUALITY_THRESHOLD=1 なので、役割ごとの日割り額の差で status はすぐ alert になる。

echo "== Phase 5: @all で点呼（tui --say。mgr/dev_m/dev_f/chat が #会議室 に応答） =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --say "@all 点呼です"
echo
sleep 3

echo "== Phase 5: 未知の宛先 @nobody（#会議室 に「宛先不明」の system 通知） =="
OFFICE_SERVER_URL="${BASE}" "${ROOT}/client/target/debug/tui" --say "@nobody やあ"
echo
sleep 2

echo "== Phase 5: say の宛先（server.log を確認。target=all / target=nobody） =="
grep -a 'say を受信' "${OUT}/server.log" | tail -n 4 || true

echo "== Phase 5.1: GET /api/ledger（全社員。残高 0 の chat も含む） =="
curl -fsS "${HTTP}/api/ledger"; echo

echo "== Phase 5.1: GET /api/ledger/mgr/entries?limit=5（mgr の元帳履歴） =="
curl -fsS "${HTTP}/api/ledger/mgr/entries?limit=5"; echo

echo "== Phase 5.3: GET /api/economy/status（残高分布・格差の観察） =="
curl -fsS "${HTTP}/api/economy/status"; echo

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
echo "== Phase 5 サマリ（経済・社会） =="
echo "say target=all   : $(grep -ac 'target=all' "${OUT}/server.log" || true) 件"
echo "say target=nobody: $(grep -ac 'target=nobody' "${OUT}/server.log" || true) 件"
echo "ledger           : $(curl -fsS "${HTTP}/api/ledger")"
echo "economy status   : $(curl -fsS "${HTTP}/api/economy/status")"

echo
echo "SMOKE OK"
