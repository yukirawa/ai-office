#!/usr/bin/env sh
# クライアント側（ターミナル2）。worker（dev_m / dev_f）と TUI を起動する。
#
# サーバー側は別ターミナルで `sh scripts/server.sh` を動かしておく。
# 実デプロイと同じく、OFFICE_SERVER_URL で接続先（Tailscale のアドレス等）を指定できる。
# TUI を終了（q / Ctrl-C）すると worker も停止する。
#
#   sh scripts/client.sh
#   OFFICE_SERVER_URL=ws://100.x.y.z:8787/ws sh scripts/client.sh
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/client/data/run"
mkdir -p "$OUT"

# ルートの .env を読み込む。既に export 済みの環境変数を優先する（.env は上書きしない）。
load_env_file() {
  file="$1"
  [ -f "$file" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      '' | '#'*) continue ;;
    esac
    line=${line#export }
    key=${line%%=*}
    [ "$key" = "$line" ] && continue
    val=${line#*=}
    key=$(printf '%s' "$key" | tr -d '[:space:]')
    val=$(printf '%s' "$val" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
      -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//")
    [ -z "$key" ] && continue
    eval "existing=\${$key:-}"
    [ -n "$existing" ] && continue
    export "$key=$val"
  done < "$file"
}
load_env_file "$ROOT/.env"

SERVER_URL="${OFFICE_SERVER_URL:-ws://127.0.0.1:8787/ws}"
WS="$OUT/ws"

echo "== build client =="
(cd "$ROOT/client" && cargo build --workspace -q)

W1=""
W2=""
cleanup() {
  [ -n "$W1" ] && kill "$W1" 2>/dev/null || true
  [ -n "$W2" ] && kill "$W2" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

mkdir -p "$WS/dev_m" "$WS/dev_f"
echo "== workers 起動（接続先: ${SERVER_URL}） =="
OFFICE_SERVER_URL="$SERVER_URL" OFFICE_EMPLOYEE_ID=dev_m OFFICE_DEVICE_ID=zenbook \
  OFFICE_WORKSPACE="$WS/dev_m" \
  "$ROOT/client/target/debug/worker" > "$OUT/dev_m.log" 2>&1 &
W1=$!
OFFICE_SERVER_URL="$SERVER_URL" OFFICE_EMPLOYEE_ID=dev_f OFFICE_DEVICE_ID=zenbook \
  OFFICE_WORKSPACE="$WS/dev_f" \
  "$ROOT/client/target/debug/worker" > "$OUT/dev_f.log" 2>&1 &
W2=$!
sleep 1

echo
echo "== TUI 起動（q または Ctrl-C で終了） =="
echo "ログ: $OUT/dev_m.log, $OUT/dev_f.log"
OFFICE_SERVER_URL="$SERVER_URL" OFFICE_TUI_ID="${OFFICE_TUI_ID:-owner}" \
  "$ROOT/client/target/debug/tui"
