#!/usr/bin/env sh
# サーバー/クライアントのログを解析して、動作をざっくり判定する。
#
#   sh scripts/logcheck.sh                 # 既定: server/data/smoke
#   sh scripts/logcheck.sh <dir>           # server.log / dev_*.log のあるディレクトリ
#
# 判定は目安: panic / level=ERROR が無ければ PASS。failed タスクや WARN は内容を確認。
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${1:-$ROOT/server/data/smoke}"
SRV="$DIR/server.log"

if [ ! -f "$SRV" ]; then
  echo "サーバーログが見つかりません: $SRV"
  echo "先に 'sh scripts/smoke.sh' を実行するか、対象ディレクトリを引数で指定してください。"
  exit 1
fi

count() { grep -ac "$1" "$SRV" 2>/dev/null || true; }

# count_all はサーバーログに加え worker のログ（dev_*.log）も横断して数える。
# 「宛先不明」など #会議室 への通知は notice として配信され、
# サーバーログには出ないが、接続中の worker のログには残るため。
count_all() {
  grep -ah "$1" "$SRV" "$DIR"/dev_m.log "$DIR"/dev_f.log 2>/dev/null | wc -l | tr -d ' '
}

echo "== 解析対象: $SRV =="
printf 'check-in              : %s\n' "$(count 'msg=check-in')"
printf 'check-out             : %s\n' "$(count 'msg=check-out')"
printf 'task_assign 送信      : %s\n' "$(count 'task_assign を送信')"
printf 'task_result 受信      : %s\n' "$(count 'task_result を受信')"
printf 'say 受信              : %s\n' "$(count 'say を受信')"
printf '日割り給与の支給      : %s\n' "$(count '日割り給与を支給')"
printf '雑談(chat)投稿のきっかけ: %s\n' "$(count 'chat 役へメッセージ')"
printf '格差の観察            : %s\n' "$(count_all '格差')"
printf '宛先不明の通知        : %s\n' "$(count_all '宛先不明')"

echo
echo "== タスクの最終状態（done / failed） =="
if grep -aq 'status=done\|status=failed' "$SRV"; then
  grep -a 'status=done\|status=failed' "$SRV" | sed -e 's/.*msg=//' | sort | uniq -c
else
  echo "(遷移ログなし)"
fi

echo
echo "== エラー / 警告 =="
for lvl in ERROR WARN; do
  if grep -aq "level=$lvl" "$SRV"; then
    printf '[%s]\n' "$lvl"
    grep -a "level=$lvl" "$SRV" | head -n 5
  else
    printf '%s なし\n' "$lvl"
  fi
done

echo
echo "== クライアントログ =="
echo
printf 'check-out 理由        : '
grep -a 'msg=check-out' "$SRV" | sed -e 's/.*reason=//' | sort | uniq -c | tr '\n' ' '; echo

for f in "$DIR"/dev_m.log "$DIR"/dev_f.log; do
  [ -f "$f" ] || continue
  printf -- '- %s: ' "$(basename "$f")"
  if grep -aq 'panicked\|ERROR' "$f"; then
    echo "エラー/panic を含む"
  elif grep -aq 'shutdown complete\|sent bye' "$f"; then
    echo "正常終了"
  else
    echo "終了ログ未確認"
  fi
done

echo
echo "== 判定 =="
if grep -aq 'panic' "$SRV"; then
  echo "FAIL: panic を検出しました"
  exit 2
elif grep -aq 'level=ERROR' "$SRV"; then
  echo "WARN: ERROR 行があります（上の内容を確認してください）"
else
  echo "PASS: panic / ERROR なし"
fi
