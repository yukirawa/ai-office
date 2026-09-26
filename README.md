# ai-office

自宅鯖に住むAI社員の仮想オフィス。

- **サーバー（Go / `officed`）** = 頭脳・記憶・調整。常時稼働。
- **クライアント（Rust）** = 手足・実行。必要な時だけアウトバウンド WebSocket で接続。
- **あなた** = オーナー（最上位）。
- **AI社員** = `mgr`（管理職）／`dev_m`・`dev_f`（平社員）／`chat`（雑談・日雇い）。

設計の詳細は [`priject.md`](./priject.md) を参照。プロトコルは §4、データモデルは §5。

## 構成

```
server/                 Go 製 officed（頭脳・記憶・調整）
  cmd/officed/          起動配線
  internal/
    config/             環境変数・初期社員・給与
    presence/           出退勤レジストリ（在席）
    api/                HTTP / WebSocket ハンドラ + ブロードキャスト
    store/              SQLite（employees/sessions/messages/ledger/relationships/tasks）
    economy/            学の元帳
    llm/                LLM HTTP ラッパー（Anthropic + mock）
    agents/             AI社員 goroutine（Phase 1 は mgr の計画のみ）
    persona/            キャラ設定
    gh/                 GitHub 連携（Phase 3、現在は 501 スタブ）

client/                 Rust workspace
  crates/protocol/      共通メッセージ型（worker と tui が共用）
  crates/worker/        常駐ワーカー（hello → heartbeat → bye）
  crates/tui/           read-only TUI ビューア

scripts/                demo.sh（対話）, smoke.sh（自動E2E）
secrets/                APIキー等（git 管理外）
```

## ビルド

```sh
# サーバー
cd server && go build ./...

# クライアント
cd client && cargo build --workspace
```

## 動かす（Phase 0: TUI で出退勤を体感）

いちばん手軽なのは対話デモ。`officed` と worker 2 体（`dev_m` / `dev_f`）を起動し、
そのまま TUI を表示する。

```sh
sh scripts/demo.sh
# OFFICE_PORT=9000 sh scripts/demo.sh   # ポートを変える場合
```

TUI のキー操作は `q`（または `Ctrl-C`）で終了。終了すると worker と `officed` も止まる。

手動で起動する場合:

```sh
# 1) サーバー
cd server
OFFICE_LLM_PROVIDER=mock go run ./cmd/officed

# 2) worker（別ターミナル）
cd client
OFFICE_EMPLOYEE_ID=dev_m OFFICE_DEVICE_ID=zenbook ./target/debug/worker

# 3) TUI（別ターミナル）
cd client
./target/debug/tui
```

タスクを mgr に投入すると計画が `#会議室` に投稿される（Phase 1.3 の確認）:

```sh
curl -X POST http://127.0.0.1:8787/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"ログイン画面の実装","description":"...","from":"owner"}'
```

## 自動E2E

```sh
sh scripts/smoke.sh
```

`officed` 起動 → worker 2 体接続 → 出退勤・presence・タスク計画・日割り給与 cron を
確認し、TUI のヘッドレススナップショット（`tui --snapshot`）を表示する。

## テスト

```sh
cd server && go test ./...
cd client && cargo test --workspace
```

## 主な環境変数（サーバー）

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `OFFICE_ADDR` | `:8787` | 待ち受けアドレス |
| `OFFICE_DB` | `officed.db` | SQLite ファイル |
| `OFFICE_HEARTBEAT_TIMEOUT` | `90s` | 無応答を退勤扱いにするまで |
| `OFFICE_PAYROLL_CRON` | `0 9 * * *` | 日割り給与の cron（`@every 5s` も可） |
| `OFFICE_PAYROLL_TZ` | `Asia/Tokyo` | cron のタイムゾーン |
| `OFFICE_LLM_PROVIDER` | `mock` | `mock` または `anthropic` |
| `OFFICE_LLM_MODEL` | `claude-3-5-haiku-latest` | モデル名 |
| `ANTHROPIC_API_KEY` | （空） | 未設定なら `secrets/anthropic.key` を読む。無ければ mock |

クライアント（worker / tui）は `OFFICE_SERVER_URL`（既定 `ws://127.0.0.1:8787/ws`）を使う。

## エンドポイント（§4.3）

| メソッド | パス | 説明 |
| --- | --- | --- |
| GET | `/healthz` | 死活監視 |
| GET | `/ws` | WebSocket（worker / TUI） |
| GET | `/api/employees` | 社員一覧 + 在席状態 |
| GET | `/api/ledger/:id` | 学の残高 |
| GET | `/api/relationships/:id` | 関係値 |
| POST | `/api/tasks` | オーナーからのタスク投入（mgr に計画させる） |
| POST | `/webhook/github` | GitHub webhook（Phase 3、現在 501） |

## 現在のフェーズ

- **Phase 0（完了）**: protocol crate / presence + WS / worker 接続ループ / TUI read-only
- **Phase 1（完了）**: SQLite store / LLM HTTP ラッパー（Anthropic + mock）/ mgr エージェント（計画のみ）/ 日割り給与 cron
- 次は Phase 2（手足: Local モード, `task_assign`, bubblewrap, `dev_m` エージェント）
