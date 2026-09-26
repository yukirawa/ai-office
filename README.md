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

## 設定（.env）

リポジトリ直下の `.env` に設定をまとめられる（`officed` が起動時に自動で読み込む。既に export
した環境変数が優先される）。

```sh
cp .env.example .env
# 実キーを使うなら OFFICE_LLM_PROVIDER=anthropic と ANTHROPIC_API_KEY を設定
```

## 2ターミナルで動かす（デプロイ相当）

実デプロイは「サーバーは常時稼働、クライアントは必要な時だけ接続」する構成なので、
ターミナルを 2 つ使うのが基本形。

ターミナル1（サーバー側 = 頭脳・記憶）:

```sh
start-server
```

ターミナル2（クライアント側 = 手足・実行）:

```sh
start-client
# 別ホストの鯖へ繋ぐ場合: OFFICE_SERVER_URL=ws://<tailscale-ip>:8787/ws start-client
```

- `start-server` / `start-client` は `~/.local/bin` に入れたショートカット（実体はリポジトリ直下の
  `./start-server` / `./start-client` → `scripts/*.sh`）。PATH に無ければ `./start-server` を使う。
- サーバーはルートの `.env` を自動で読む。クライアント側も `.env` を読む（既存 env が優先）。
- TUI は `q` / `Ctrl-C` で終了。起動した worker（dev_m / dev_f）は終了時に自動停止する。
- ログ: サーバーは画面、クライアントは `client/data/run/*.log`。

## 1ターミナルで手軽に試す

```sh
sh scripts/demo.sh
# OFFICE_PORT=9000 sh scripts/demo.sh   # ポートを変える場合
```

`officed` と worker 2 体を起動して TUI を表示する。終了すると全部止まる。
TUI は 4 ペイン（左: `社員` / `タスク`、中央: `#会議室`、右: `mgr`）と下部の `> _`。

## 手動で起動する

```sh
# ターミナル1: サーバー
cd server && go run ./cmd/officed

# ターミナル2: worker
cd client
OFFICE_EMPLOYEE_ID=dev_m OFFICE_WORKSPACE="$PWD/data/run/ws/dev_m" ./target/debug/worker

# ターミナル3: TUI
cd client && ./target/debug/tui
```

タスクを mgr に投入すると、mgr が計画 → dev エージェントが worker に割当 → worker が実行、
という流れで処理される（Phase 1.3 / 2 の確認）:

```sh
curl -X POST http://127.0.0.1:8787/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"ログイン画面の実装","description":"...","from":"owner"}'
```

worker は Local モードで `OFFICE_WORKSPACE` の中にファイルを書き、結果を返す。
状態遷移は `GET /api/tasks` で確認できる:

```sh
curl http://127.0.0.1:8787/api/tasks?limit=5
# pending -> assigned -> working -> review -> done
```

## タスク実行の仕組み（Phase 2）

```
POST /api/tasks
  -> mgr: LLM で計画を立て #会議室 に投稿
  -> mgr: dev_m / dev_f にラウンドロビンで割当（tasks.status=assigned）
  -> dev: 計画を JSON アクションに変換して task_assign を送信（status=working）
  -> worker: OFFICE_WORKSPACE 内で実行（bubblewrap サンドボックス）
  -> worker: task_result を返す（status=review）
  -> mgr: レビューして done / failed
```

worker の実行モード:

- **local**: `{"actions":[{"op":"read|write|list|exec",...}]}` をワークスペース内で実行。
  パスはワークスペース外へ出られない（`..` や絶対パスは拒否）。`exec` は既定で無効。
- **remote**: GitHub REST API でブランチ作成 → コミット → PR 作成。サーバーが発行した
  短命の installation token を `task_assign` の payload で受け取る。worker がオフラインの
  ときはサーバー側（`gh`）が PR を作る。

## 関係値と雑談（Phase 4）

- **関係値（4.2）**: タスクの成功／失敗で mgr と担当者の `affinity`（-100〜+100）と `trust`（0〜100）が
  変化する。`GET /api/relationships/:id` と TUI 右ペインで確認できる。
- **chat 役（4.3）**: 雑談・雑用役。**Ollama は使わず**サーバーの LLM クライアントで応答する。
  オーナーは次のように話しかけられる:
  ```sh
  curl -X POST http://127.0.0.1:8787/api/chat \
    -H 'Content-Type: application/json' -d '{"message":"おはよう"}'
  ```
- **雑談 cron（4.4）**: `OFFICE_CHAT_CRON` の間隔で chat 役が `#会議室` に一言投稿する
  （`off` で無効）。

## 実APIキーで動かす（DeepSeek / Anthropic）

既定は `mock`（外部 API を呼ばない）。実キーを使うと mgr / dev / chat が実際に推論する。
プロバイダは `OFFICE_LLM_PROVIDER=deepseek`（ネイティブ対応）または `anthropic`。

### DeepSeek

```sh
# .env に書くのが簡単（推奨）:
#   OFFICE_LLM_PROVIDER=deepseek
#   DEEPSEEK_API_KEY=sk-xxxxxxxx
# env や secrets/deepseek.key でも可（gitignore 済み）:
#   mkdir -p secrets && printf '%s\n' 'sk-xxxxxxxx' > secrets/deepseek.key

# 聴通とモデル確認（サーバ起動不要）
cd server
OFFICE_LLM_PROVIDER=deepseek go run ./cmd/officed --llm-check
OFFICE_LLM_PROVIDER=deepseek go run ./cmd/officed --llm-models
```

既定モデルは `deepseek-chat`（`OFFICE_LLM_MODEL` 未設定時）。`deepseek-reasoner` も指定可能。

### Anthropic

```sh
# .env:  OFFICE_LLM_PROVIDER=anthropic / ANTHROPIC_API_KEY=sk-ant-...
cd server
OFFICE_LLM_PROVIDER=anthropic go run ./cmd/officed --llm-check
OFFICE_LLM_PROVIDER=anthropic go run ./cmd/officed --llm-models
```

### 起動中に確認（共通）

```sh
curl http://127.0.0.1:8787/api/llm
curl -X POST http://127.0.0.1:8787/api/llm/ping -H 'Content-Type: application/json' -d '{}'
```

注意:

- モデル名はプロバイダ側で変わる。`404` なら `--llm-models` で確認して `OFFICE_LLM_MODEL` を設定する
  （404 のエラーにもその旨ヒントを出します）。
- `401` のときはキーの設定場所（env / `.env` / `secrets/*.key`）を確認する。
- タイムアウトは `OFFICE_LLM_TIMEOUT`（既定 120s）、1 リクエストの最大出力は
  `OFFICE_LLM_MAX_TOKENS`（既定 1024）。429/5xx は自動リトライ（既定 3 回）。

## GitHub 連携（Phase 3・任意）

GitHub App を用意して以下の環境変数を設定すると有効になる。未設定なら連携せず起動する。

| 変数 | 説明 |
| --- | --- |
| `GITHUB_APP_ID` | GitHub App の ID |
| `GITHUB_INSTALLATION_ID` | インストール ID |
| `GITHUB_APP_PRIVATE_KEY_PATH` | 秘密鍵 PEM（既定 `secrets/github-app.pem`。`GITHUB_PRIVATE_KEY` で直接渡してもよい） |
| `GITHUB_WEBHOOK_SECRET` | webhook 署名検証用（HMAC-SHA256） |
| `GITHUB_REPO` | 既定の対象リポジトリ `owner/name` |
| `GITHUB_BASE_BRANCH` | PR のベースブランチ（既定 `main`） |

App の権限は `repo`, `pull_requests`, `issues` のみ（§9）。webhook で `issues.opened` を受けると
remote タスクに変換され、PR 作成まで進む。署名は `X-Hub-Signature-256` で検証する。

## テスト手順（一貫した確認）

### 1. 単体テスト

```sh
cd server && go test ./...           # presence / store / economy / llm / agents / gh / api
cd client && cargo test --workspace  # protocol / worker / tui
```

`go test -race ./...` も可。

### 2. 自動E2E（一発で通し確認・推奨）

```sh
sh scripts/smoke.sh                  # ポート 18787。OFFICE_SMOKE_PORT で変更可
```

`officed` 起動 → worker 2 体接続 → 出退勤・presence・タスク計画→実行（Local モードで実際に
ファイル作成）・chat 応答・関係値変動・日割り給与/雑談 cron を確認し、TUI のヘッドレス
スナップショット（`tui --snapshot`）を表示する。出力は `server/data/smoke/`（`server.log`,
`dev_m.log`, `ws/`）に残る。最後に `SMOKE OK` が出れば成功。

### 3. 手動で起動して TUI で確認

ターミナル A（サーバー）:

```sh
cd server
OFFICE_LLM_PROVIDER=mock OFFICE_PAYROLL_CRON='@every 5s' OFFICE_CHAT_CRON='@every 5s' \
  go run ./cmd/officed
```

ターミナル B（worker: dev_m）:

```sh
cd client
mkdir -p ../server/data/ws/dev_m
OFFICE_EMPLOYEE_ID=dev_m OFFICE_WORKSPACE="$PWD/../server/data/ws/dev_m" \
  cargo run -p worker
```

ターミナル C（worker: dev_f）:

```sh
cd client
mkdir -p ../server/data/ws/dev_f
OFFICE_EMPLOYEE_ID=dev_f OFFICE_WORKSPACE="$PWD/../server/data/ws/dev_f" \
  cargo run -p worker
```

ターミナル D（TUI）:

```sh
cd client && cargo run -p tui        # q または Ctrl-C で終了
```

（A〜D をまとめて起動するだけなら `sh scripts/demo.sh` が楽。`OFFICE_PORT` でポート変更可）

### 4. 動作確認（別ターミナルから）

```sh
curl http://127.0.0.1:8787/healthz
curl http://127.0.0.1:8787/api/employees

# タスクを投入 -> TUI の「タスク」ペインと #会議室 が動く
curl -X POST http://127.0.0.1:8787/api/tasks -H 'Content-Type: application/json' \
  -d '{"title":"ログイン画面の実装","description":"テスト","from":"owner"}'
curl http://127.0.0.1:8787/api/tasks

# chat 役に話しかける
curl -X POST http://127.0.0.1:8787/api/chat -H 'Content-Type: application/json' \
  -d '{"message":"おはよう"}'
```

期待される流れ:

- タスク: `pending → assigned → working → review → done`
- `#会議室`: mgr の計画 → dev_* の実行結果 → mgr のレビュー、chat 役の雑談
- 生成物: `server/data/ws/<employee>/reports/<task_id>.md`
- TUI 右ペインの mgr 関係値が成功で上がる（`dev_m +n`）

### 5. TUI の表示項目

左 `社員` / 左下 `タスク` / 中央 `#会議室` / 右 `mgr`（学残高・状態・関係値）+ 下部 `> _`。
ヘッドレス確認は `cd client && cargo run -p tui -- --snapshot`（または `OFFICE_TUI_SNAPSHOT=1`）。

## 主な環境変数（サーバー）

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `OFFICE_ADDR` | `:8787` | 待ち受けアドレス |
| `OFFICE_DB` | `officed.db` | SQLite ファイル |
| `OFFICE_HEARTBEAT_TIMEOUT` | `90s` | 無応答を退勤扱いにするまで |
| `OFFICE_PAYROLL_CRON` | `0 9 * * *` | 日割り給与の cron（`@every 5s` も可） |
| `OFFICE_PAYROLL_TZ` | `Asia/Tokyo` | cron のタイムゾーン |
| `OFFICE_CHAT_CRON` | `0 * * * *` | 雑談 cron（`off` で無効） |
| `OFFICE_LLM_PROVIDER` | `mock` | `mock` / `deepseek` / `anthropic` |
| `OFFICE_LLM_MODEL` | プロバイダ既定 | モデル名（deepseek-chat / claude-3-5-haiku-latest） |
| `OFFICE_LLM_TIMEOUT` | `120s` | 1 リクエストのタイムアウト |
| `OFFICE_LLM_MAX_TOKENS` | `1024` | 1 リクエストの最大出力トークン |
| `DEEPSEEK_API_KEY` | （空） | 未設定なら `secrets/deepseek.key` を読む |
| `OFFICE_DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | DeepSeek API のベース URL |
| `ANTHROPIC_API_KEY` | （空） | 未設定なら `secrets/anthropic.key` を読む |
| `GITHUB_*` | （空） | 上記「GitHub 連携」を参照 |

クライアント（worker / tui）は `OFFICE_SERVER_URL`（既定 `ws://127.0.0.1:8787/ws`）を使う。
worker の追加設定:

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `OFFICE_EMPLOYEE_ID` | （必須） | 社員 ID（`dev_m` / `dev_f` / `mgr`） |
| `OFFICE_DEVICE_ID` | hostname | 端末名 |
| `OFFICE_WORKSPACE` | （空） | local モードで読み書きする作業ディレクトリ |
| `OFFICE_SANDBOX` | `bwrap` | `bwrap` または `none` |
| `OFFICE_ALLOW_EXEC` | `0` | `1` で `exec` アクションを許可 |
| `OFFICE_GITHUB_API_URL` | `https://api.github.com` | remote モードの API ベース |

## エンドポイント（§4.3）

| メソッド | パス | 説明 |
| --- | --- | --- |
| GET | `/healthz` | 死活監視 |
| GET | `/ws` | WebSocket（worker / TUI） |
| GET | `/api/employees` | 社員一覧 + 在席状態 |
| GET | `/api/ledger/:id` | 学の残高 |
| GET | `/api/relationships/:id` | 関係値 |
| POST | `/api/tasks` | オーナーからのタスク投入（`title`/`description`/`from`/`mode`/`repo`/`base_branch`） |
| GET | `/api/tasks` | タスク一覧（`?status=&limit=`） |
| POST | `/api/chat` | オーナーの発言に chat 役が応答 |
| GET | `/api/llm` | LLM プロバイダ/モデル/設定の表示 |
| POST | `/api/llm/ping` | LLM へ 1 回だけ問い合わせて疎通確認 |
| POST | `/webhook/github` | GitHub webhook（署名検証 → タスク化） |

## 現在のフェーズ

- **Phase 0（完了）**: protocol crate / presence + WS / worker 接続ループ / TUI read-only
- **Phase 1（完了）**: SQLite store / LLM HTTP ラッパー（Anthropic + mock）/ mgr エージェント（計画）/ 日割り給与 cron
- **Phase 2（完了）**: `task_assign`/`task_result` / worker Local モード（ファイル操作）/ bubblewrap サンドボックス / dev_m・dev_f エージェント（mgr がラウンドロビン割当 → レビュー）
- **Phase 3（実装済み・要資格情報）**: GitHub App 認証（JWT → installation token）/ worker Remote モード（ブランチ → コミット → PR）/ webhook 署名検証 → タスク化 / worker 不在時のサーバー側 PR 作成
- **Phase 4（完了）**: ペルソナ（既存 + chat 役）/ 関係値システム（タスク結果で変動）/ chat 役（Ollama なし）/ 雑談 cron / TUI のタスクペイン
- 次は Phase 5（学の元帳・残高API、高級モデル購入、労働運動トリガーの観察）
