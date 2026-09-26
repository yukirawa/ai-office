#!/usr/bin/env sh
# サーバー側（ターミナル1）。プロジェクトルートで officed を起動する。
#
# 設定はルートの .env から officed が自動で読み込む（既に export 済みの環境変数が優先）。
# 停止は Ctrl-C。
#
#   sh scripts/server.sh
#   OFFICE_ADDR=127.0.0.1:9000 sh scripts/server.sh   # 一時的に上書き
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/server/data/run"
mkdir -p "$OUT"

echo "== build officed =="
(cd "$ROOT/server" && go build -o "$OUT/officed" ./cmd/officed)

if [ -f "$ROOT/.env" ]; then
  echo "== .env を読み込みます（officed が自動で読みます） =="
else
  echo "== .env がありません。cp .env.example .env で作成できます =="
fi

echo "== officed 起動（Ctrl-C で停止） =="
cd "$ROOT"
exec "$OUT/officed"
