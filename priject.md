# ai-office 設計書 v1.0

自宅鯖に住むAI社員の仮想オフィス。

---

## 0. この文書の扱い

これは設計意図を伝える文書であり、実装の細部を縛るものではない。

実装者が「この設計だと問題がある」「もっといい方法がある」と判断した場合、変更してよい。ただし：

- 変更した理由をコードコメントか、この文書に残す
- プロトコル（§4）とデータモデル（§5）を変える場合は、先にこの文書を更新する
- 内部実装の選択は自由

固定するもの：プロトコル、データモデル、ディレクトリ境界
裁量に委ねるもの：ライブラリ選定、内部API、並行モデル、エラー処理、テスト手法

---

## 1. プロジェクト概要

目的：自宅鯖に住むAI社員の仮想オフィスを構築する。

本質：
- サーバー（Go）＝ 頭脳・記憶・調整
- クライアント（Rust）＝ 手足・実行
- あなた ＝ オーナー（最上位）
- AI社員 ＝ 管理職・平社員・雑談役

段階的ゴール：
1. TUIで「AI社員が出勤・退勤する」を体感
2. AI同士がタスクを割振り・実行・レビュー
3. ペルソナ・関係値・恋愛・経済を実験
4. Web UI → 2Dピクセルオフィスへ具現化

非ゴール（初期段階でやらない）：
- 外部公開
- モバイルネイティブアプリ

---

## 2. 全体アーキテクチャ

[あなた / スマホ / TUI]
        ↓ WebSocket (Tailscale内)
[自宅鯖: Ryzen 5 3500U / 20GB / Arch]
  officed (Go)
   ├ presence     出退勤・ハートビート
   ├ agents       AI社員goroutine
   ├ llm          自作HTTPラッパー
   ├ economy      学の元帳
   ├ gh           GitHub App / webhook
   ├ persona      キャラ・関係値
   ├ store        SQLite
   └ api          HTTP/WS
        ↑ WebSocket (アウトバウンド)
[Zenbook: Core Ultra 9 285H / 32GB / EndeavourOS]
  worker (Rust)
   ├ Local モード  直接ファイル編集
   ├ Remote モード GitHub API経由
   ├ bubblewrap    サンドボックス
   └ Ollama        雑談役（オンデマンド）

原則：
- サーバーは常時稼働、クライアントは必要な時だけ接続
- 通信はアウトバウンドWebSocket（NAT/ポート開放不要）
- PCオフライン時：頭脳だけ働き、作業はGitHub上で完結
- 認証・秘匿：Tailscale内に閉じる

---

## 3. 組織と社員

ID        役割                      性別  住処            基本給
mgr       管理職・あなたの窓口      男    鯖常駐          500学/月
dev_m     平社員（開発・レビュー）  男    Zenbook         350学/月
dev_f     平社員（開発・レビュー）  女    Zenbook         350学/月
chat      雑談・雑用（日雇い）      未定  Zenbook(Ollama) 日当制

階層：
あなた（オーナー）
  └ mgr（窓口・現場責任者）
      └ dev_m / dev_f / chat

初期段階では３人（+１人）の社員AIとする。
役割は固定しない：mgrが動的に割り振る。

初期実装では給与固定、歩合なし。AIが自発的に不公平を訴えたら実装する（創発実験）。

補足: `mgr` と `chat` はサーバー側の goroutine で動く常駐社員で、クライアント接続を持たない。
presence 上は起動直後から常時在席として登録し（`MarkResident`）、heartbeat が無くても退勤扱いに
しない（§16.4）。

---

## 4. プロトコル仕様（固定）

トランスポート：WebSocket、JSON、UTF-8
エンコード：tag = "type"、フィールド名は snake_case

### 4.1 Client → Server

接続直後に1回：
{ "type": "hello", "employee_id": "dev_m", "device_id": "zenbook", "version": "0.1.0" }

定期送信（間隔は実装裁量、推奨30秒）：
{ "type": "heartbeat", "ts": "2026-09-26T12:34:56Z" }

明示的退勤（切断でも可）：
{ "type": "bye", "reason": "shutdown" }

### 4.2 Server → Client

helloへの応答：
{
  "type": "welcome",
  "session_id": "uuid-v4",
  "server_time": "2026-09-26T12:34:56Z",
  "office": { "online": ["mgr", "dev_m"] }
}

チャンネルへの投稿：
{
  "type": "notice",
  "channel": "#会議室",
  "from": "mgr",
  "text": "今日のタスクは…",
  "ts": "2026-09-26T12:34:56Z"
}

タスク割当（将来）：
{ "type": "task_assign", "task_id": "uuid-v4", "payload": {} }

エラー：
{ "type": "error", "code": "invalid_hello", "message": "expected hello" }

裁量：
- 追加フィールドは無視される（forward compatibility）
- 新しい type の追加は自由（既存を壊さなければ）
- payload の中身は Phase 2 以降で決める

### 4.3 エンドポイント

/healthz         GET        死活監視
/ws              WebSocket  worker/TUI 接続
/api/employees   GET        社員一覧（将来）
/api/ledger/:id  GET        学の残高（将来）
/webhook/github  POST       GitHub webhook

---

## 5. データモデル（SQLite / 固定）

テーブル：employees, sessions, messages, ledger, relationships, tasks

employees
- id (PK), name, role, gender, device_id, persona_json, created_at
- role: 'manager' | 'worker' | 'chat'

sessions
- id (PK), employee_id (FK), device_id, started_at, ended_at, end_reason

messages
- id (PK自動), channel, from_id, to_id, content, ts

ledger
- id (PK自動), employee_id, amount, reason, ts
- amount: 正=収入, 負=支出

relationships
- from_id, to_id (複合PK), affinity (-100〜+100), trust (0〜100), updated_at

tasks
- id (PK), title, description, status, assignee, created_by, result, created_at, updated_at
- status: 'pending' | 'assigned' | 'working' | 'review' | 'done' | 'failed'

裁量：
- インデックス追加は自由
- カラム追加はOK（削除・型変更はこの文書を先に更新）
- マイグレーション方法は実装裁量

---

## 6. サーバー設計（Go）

### 6.1 ディレクトリ（固定）

server/
├── go.mod
├── cmd/officed/main.go
└── internal/
    ├── config/     環境変数・設定
    ├── presence/   出退勤レジストリ
    ├── api/        HTTP/WS ハンドラ
    ├── store/      SQLiteアクセス
    ├── llm/        自作HTTPラッパー
    ├── agents/     AI社員goroutine
    ├── economy/    学の元帳
    ├── gh/         GitHub連携
    └── persona/    キャラ・関係値

裁量：
- パッケージ追加はOK
- 既存パッケージの統合・分割はOK（理由を残すこと）

### 6.2 主要な型（意図）

presence パッケージ：
- Status: online / busy / break / offline
- Employee: ID, DeviceID, Status, LastSeen
- Registry: 社員の在席を管理する。CheckIn, CheckOut, Online を持つ

llm パッケージ：
- Client インターフェース：Chat(ctx, Request) (Response, error)
- Request: Model, System, Messages, Tools, MaxTokens
- Message: Role, Content

agents パッケージ：
- Employee: ID, Persona, Inbox (chan Task), State
- Task: ID, Title, Description, From

裁量：
- 並行モデル（Mutex / channel / actor）は実装裁量
- 状態機械の表現方法は自由
- プロバイダごとの実装は自由（Anthropic / OpenAI / Ollama）
- テスト用のモック差し替えは必須（Client をインターフェースにしている理由）

### 6.3 エージェントモデル（意図）

1体 = 1 goroutine + channel を推奨。ただし強制ではない。

ループ構造：
Inbox からタスク受信 → think → work → report → idle を繰り返す。
Context の Done で終了。

dev の local 実行は、このループ内で plan（次の一手を LLM に問う）→ execute（worker で実行）
→ observe（結果を次の問いに渡す）を `{"done":true}` まで反復する（§16.6）。

無限ループ防止（必須）：
- 最大ターン数
- 予算上限（トークン数）
- 同一結論の繰り返し検出

---

## 7. クライアント設計（Rust）

### 7.1 workspace構成（固定）

client/
├── Cargo.toml
└── crates/
    ├── protocol/
    ├── worker/
    └── tui/

裁量：
- crate追加はOK
- protocol は worker と tui 双方から使う（この境界は固定）

### 7.2 protocol crate

ClientMsg enum：Hello, Heartbeat, Bye
ServerMsg enum：Welcome, Notice, TaskAssign, Error
Office 構造体：online (Vec<String>)

serde derive、tag = "type"、rename_all = "snake_case"。

裁量：
- 未知の type は Unknown variant にフォールバックしてもよい
- Result 型でのラップ、エラー型の定義は自由

### 7.3 worker crate

責務：
- 起動時に鯖へWebSocket接続
- hello 送信 → welcome 受信
- 定期的に heartbeat（推奨30秒）
- 受信メッセージを処理（Phase 0ではログ出力のみ）
- シグナル受信で bye 送信 → 切断

将来：
- task_assign 受信 → Local/Remoteモードで実行
- bubblewrap でサンドボックス
- Ollama 連携（localhost:11434）

裁量：
- 再接続戦略（指数バックオフ、最大リトライ回数）
- シグナルハンドリング
- ログ出力先

### 7.4 tui crate

レイアウト意図：

┌─社員───────┬─#会議室────────────┬─mgr──────────┐
│● mgr  在席 │ mgr: 今日のタスクは…  │ 学: 512       │
│● dev_m在席 │ dev_f: 了解です！     │ 状態: thinking│
│○ dev_f退勤 │ chat: おはよ〜       │ 関係: dev_f+15│
│▲ chat 外出 │                     │ dev_m+3      │
└────────────┴────────────────────┴──────────────┘
> _

Phase 0 では read-only ビューアとして実装。
鯖に /ws で接続し、notice を表示するだけ。

裁量：
- ratatui を推奨するが、他ライブラリでもよい
- レイアウトの細部、色、キーバインドは自由
- 入力機能は Phase 1 以降

---

## 8. フェーズ計画

各Phaseは独立してコミット可能。エージェントに投げる単位。

Phase 0: 骨組み
- 0.1 protocol crate 実装 → crates/protocol/src/lib.rs
- 0.2 server presence + WS → internal/presence/, internal/api/ws.go
- 0.3 worker 接続ループ → crates/worker/src/main.rs
- 0.4 TUI read-onlyビューア → crates/tui/src/main.rs

受入基準：
- 0.1 cargo build -p protocol が通る
- 0.2 worker接続でログに check-in が出る
- 0.3 鯖ログに check-in/out が出る
- 0.4 出勤者リストが表示される

Phase 1: 最初の社員
- 1.1 SQLite store 実装
- 1.2 LLM HTTP ラッパー（Anthropic）
- 1.3 mgr エージェント（計画のみ）
- 1.4 日割り給与cron

Phase 2: 手足
- 2.1 Local モード（ファイル読み書き）
- 2.2 task_assign プロトコル実装
- 2.3 bubblewrap サンドボックス
- 2.4 dev_m エージェント

Phase 3: GitHub
- 3.1 GitHub App 登録・認証
- 3.2 Remote モード
- 3.3 webhook 受信 → PR作成

Phase 4: キャラクター
- 4.1 Persona システム
- 4.2 Relationship システム
- 4.3 chat 役（Ollama連携）
- 4.4 雑談cron

Phase 5: 経済・社会
- 5.1 学の元帳・残高API
- 5.3 労働運動トリガー（観察のみ）

Phase 6: GUI
- 6.1 Web UI（HTMX or React）
- 6.2 PWA化
- 6.3 2Dピクセルオフィス（Phaser）

裁量：
- フェーズ内の順序入替はOK（依存関係が許す範囲で）
- タスクの分割・統合はOK
- フェーズ自体の追加は、この文書を先に更新

---

## 9. セキュリティ・運用

- シークレット：secrets/ に置き、.gitignore 済み
- APIキー：環境変数 or secrets/*.key
- GitHub App：権限は repo, pull_requests, issues のみ
- Webhook secret：環境変数
- サンドボックス：bubblewrap、許可パスをホワイトリスト
- ネットワーク：Tailscale内のみ、外部公開しない
- バックアップ：SQLiteファイルを日次でコピー（将来）

---

## 10. テスト方針

- 単体：presence, economy, protocol（必須）
- 結合：WS接続 → check-in → heartbeat → check-out
- E2E：worker起動 → 鯖ログ確認（手動でOK）
- LLMモック：internal/llm/mock.go で API 呼ばずにテスト

CIは最初は入れない。Phase 2 まで来たら GitHub Actions で go test ./... && cargo test を追加。

裁量：
- テストの粒度、カバレッジ目標は自由
- モックライブラリの選択は自由

---

## 11. 未決定事項

実装しながら決める。エージェントは仮決めして進めてよい。

- chat 役の性別
- mgr, dev_m, dev_f の具体的な性格設定
- LLM初期モデル選定（Anthropic Haiku/Sonnet/Opus？）
- 労働運動のトリガー閾値
- 2Dオフィスのタイルセット調達
- OSS公開するか

仮決めの際の指針：
- 迷ったら「後で変えやすい方」を選ぶ
- 設定値は定数 or 環境変数にし、コードに埋め込まない
- 判断に迷ったら TODO コメントで理由を残す

---

## 12. 実装メモ（Phase 0/1）

§0 の「変更した理由を残す」に従い、固定部分（プロトコル・データモデル・ディレクトリ境界）を
壊さずに加えた裁量拡張を記録する。

### 12.1 プロトコル拡張（§4.2 の「新しい type の追加は自由」）

- Server → Client に `office_state` を追加。
  ```json
  {"type":"office_state","online":["mgr"],
   "employees":[{"id":"mgr","name":"ミカ","role":"manager","status":"online","state":"idle"}],
   "ledger":{"mgr":48},"relationships":[{"from_id":"mgr","to_id":"dev_f","affinity":15,"trust":70}],
   "ts":"..."}
  ```
  理由: TUI（read-only ビューア）が必要とする社員一覧・在席・残高・関係値を 1 通で受け取れるようにし、
  TUI から HTTP ポーリングを不要にするため。変化のたびにブロードキャストする。
  既存 type は変更していない。クライアント側は未知 type を `Unknown` に落とす前方互換を維持。
- `hello` の `employee_id` が `employees` に無い場合は「観測者（observer）」として扱い、
  在席には数えない（`welcome`/`office_state` は受け取れる）。TUI は `employee_id="owner"` で接続する。
- 接続直後に直近 50 件のメッセージを `notice` として送る（TUI が会議室を空から始めないため）。

### 12.2 エンドポイント追加（§4.3 は将来分を含む一覧）

- `GET /api/relationships/:id` — 関係値。TUI 右ペイン用。
- `POST /api/tasks` — オーナーからのタスク投入。`tasks` に保存し `mgr` の Inbox へ渡す。
  Phase 1.3（mgr の計画のみ）を単体で動作確認するための前倒し。Phase 2 の `task_assign` 実装時に整理する。

### 12.3 サーバー裁量の決定

- 在席タイムアウト: 既定 90 秒（`OFFICE_HEARTBEAT_TIMEOUT`）。無応答は退勤扱いにして `notice` を出す。
  クライアント切断時も退勤扱い（`bye` の理由はそのまま `sessions.end_reason` に記録）。
- LLM: 既定は `mock` プロバイダ。`anthropic` を選んでも API キーが無ければ mock にフォールバックし、
  オフラインでも起動できるようにした（Phase 1.2 のモック差し替え要件を兼ねる）。
- 給与: `mgr=500`, `dev_m=350`, `dev_f=350`, `chat=0`（日当制）を `config` に集約。
  日割り額は `月給 / 30`。cron 式・タイムゾーンは環境変数。
- エージェントの実行ループ防止は §6.3 の 3 種（最大ターン・トークン予算・同一結論検出）を実装済み。
- 初期社員の表示名は仮決め（§11）: `mgr=ミカ`, `dev_m=タクミ`, `dev_f=アヤナ`, `chat=チャット`。

### 12.4 クライアント裁量の決定

- `tui --snapshot`（または `OFFICE_TUI_SNAPSHOT=1`）でヘッドレスに状態サマリを出力して終了する
  モードを追加。CI や自動E2Eで描画ループを介さず検証できる。
- worker の再接続は指数バックオフ（1→2→4→…→30 秒、`OFFICE_MAX_RETRIES=0` は無限）。

---

## 13. Phase 2/3 で追加したプロトコル（§4.2 追補）

§4.2 の「新しい type の追加は自由」に従い、以下を追加する。既存 type は変更しない。

### 13.1 Client → Server

- `task_result` — ワーカーの実行結果（Phase 2.2）。
  ```json
  {"type":"task_result","task_id":"uuid","status":"done",
   "summary":"1行の要約","detail":"詳細ログ",
   "artifacts":[{"path":"reports/x.md","bytes":123}]}
  ```
  `status` は `done` | `failed`。
- `task_progress` — 実行中の進捗（Phase 2.2、任意）。
  ```json
  {"type":"task_progress","task_id":"uuid","message":"...","percent":50}
  ```

### 13.2 Server → Client

- `task_assign` の `payload` を確定（Phase 2.2）。
  ```json
  {"type":"task_assign","task_id":"uuid","payload":{
     "mode":"local",
     "title":"...",
     "reason":"mgr の計画や意図",
     "workspace":"/home/u/Dev/site",
     "actions":[{"op":"write","path":"reports/x.md","content":"..."}]
  }}
  ```
  `mode` は `local`（ファイル操作・コマンド） | `remote`（GitHub API）。
  - `local` の任意フィールド `workspace`: 作業先ディレクトリ（空なら worker の `OFFICE_WORKSPACE`）。
    worker が `OFFICE_ALLOWED_ROOTS`（+ `OFFICE_WORKSPACE`）に対して canonicalize 後に前方一致で
    検証し、許可外は拒否する。
  - `local` の `actions[]`: `{"op":"read|write|list|exec", ...}`。
    `write` は `path`+`content`、`exec` は `cmd`+`args`+`cwd`。
  - `remote` の `remote`: `{"repo","base_branch","branch","title","body","files":[{"path","content"}],"token"}`。
    `token` はサーバーが発行する短命の installation token（Tailscale 内でのみ流通）。
- `task_cancel` — 割り当て済みタスクの取り消し（予約）。
  ```json
  {"type":"task_cancel","task_id":"uuid","reason":"..."}
  ```

### 13.3 タスクの状態遷移（§5 の status を実運用）

`pending` →（mgr が担当を決定）→ `assigned` →（dev が実行開始）→ `working`
→（結果報告）→ `review` →（mgr が承認）→ `done` ／ 失敗時は `failed`。

### 13.4 データモデル追補（§5）

tasks に 3 列を追加（§5 の「カラム追加はOK」）: `mode`（既定 `local`）、`repo`、`base_branch`。

### 13.5 ワーカー（Local / Remote）の設定とサンドボックス（§7.3）

- `OFFICE_WORKSPACE` — local モードで読み書きできる作業ディレクトリ（必須）。
- `OFFICE_SANDBOX` — `bwrap`（既定） | `none`。`bwrap` が無ければ警告して `none` に落とす。
- `OFFICE_ALLOW_EXEC` — `1` のときだけ `exec` アクションを許可（既定 `0`、安全側）。
- bubblewrap は workspace のみ bind（read-write）、ネットワークは `--unshare-net`、その他は read-only。
- Remote モードは GitHub REST API を直接叩く（サーバーから渡された installation token を使う）。

### 13.6 GitHub App（Phase 3）の設定

`GITHUB_APP_ID`, `GITHUB_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY_PATH`（既定 `secrets/github-app.pem`）,
`GITHUB_WEBHOOK_SECRET`, `GITHUB_REPO`, `GITHUB_BASE_BRANCH`（既定 `main`）。
未設定なら GitHub 連携は無効のまま起動する。webhook は HMAC-SHA256（`X-Hub-Signature-256`）で検証する。

---

## 14. LLM 実運用メモ（Phase 1-4 の仕上げ）

§11「LLM初期モデル選定」を実キーで試せる形にするために、以下を実装した。

- モデル名・トークン上限の注入: `agents.Options` に `Model` / `MaxTokens` を追加し、mgr / dev / chat の
  全 `llm.Request` で使う。以前は `DefaultModel` 固定で `OFFICE_LLM_MODEL` が無視されていた。
- `internal/config`: `OFFICE_LLM_TIMEOUT`（既定 120s）、`OFFICE_LLM_MAX_TOKENS`（既定 1024）。
- `internal/llm`（Anthropic）: 可変長オプション `WithTimeout` / `WithMaxRetries`（既定 3）/
  `WithRetryBaseDelay`。リトライは 429/5xx とネットワークエラーのみ。`Retry-After` を尊重。
  401 には「API キーを確認」、404 には「モデル名を確認」のヒントを付ける。
- 診断: `officed --llm-check`（1 回の Chat で聴通確認）、`officed --llm-models`
  （`GET /v1/models` で利用可能モデル一覧、トークン消費なし）。
  起動中は `GET /api/llm` と `POST /api/llm/ping` でも確認できる。
- 既定モデルはプロバイダ依存。`deepseek` のときは `deepseek-chat`、それ以外は `claude-3-5-haiku-latest`
  （いずれも `OFFICE_LLM_MODEL` で上書き）。
- DeepSeek ネイティブ対応: `internal/llm/deepseek.go` に OpenAI 互換 API（`POST /chat/completions`,
  `GET /models`, `Authorization: Bearer`）を実装。`OFFICE_LLM_PROVIDER=deepseek` で選択。
  `Option`（`WithTimeout` / `WithMaxRetries` / `WithRetryBaseDelay`）は Anthropic と共通化した。
  `--llm-models` も DeepSeek に対応する。
- 設定の入口: プロジェクトルートの `.env`（`officed` が自動読込。既存の環境変数が優先）。
  キーは env / `.env` / `secrets/<provider>.key` のいずれでも可。
- 起動: `start-server` / `start-client`（リポジトリ直下と `~/.local/bin`）。実体は `scripts/server.sh` /
  `scripts/client.sh`。
- 終端判定のプロバイダ差異修正: 以前は Anthropic の停止理由（`end_turn` 等）しか終端と
  見なしておらず、OpenAI 互換（DeepSeek の `stop`）を「継続」と誤判定して max_turns まで
  回り続けていた。`isTerminalStop` を `stop`/`end`/`end_turn`/不明=終端、
  `length`/`max_tokens`/`tool_use`/`tool_calls`=継続に修正（Phase 5 の前に修正）。
- `OFFICE_PAYROLL_CRON` / `OFFICE_CHAT_CRON` は `off`（`none`/`disabled` 含む）で無効化できる。

---

## 15. TUI からの入力（§4.2 追補）

Phase 0 の TUI は read-only だったため、クライアント画面から発言・指示ができなかった。
オーナーが TUI から操作できるようにする（§4.2 の「新しい type の追加は自由」）。

### 15.1 Client → Server（追加）

- `say` — #会議室 への発言。
  ```json
  {"type":"say","channel":"#会議室","text":"おはよう"}
  ```
  `channel` 省略時は `#会議室`。サーバーは `from` = 接続 ID（TUI は `owner`）で `notice` を配信し、
  chat 役がいればその発言を chat に渡して応答させる。
- `task` — タスク投入（`POST /api/tasks` と同じ経路）。
  ```json
  {"type":"task","title":"...","description":"...","mode":"local","repo":"","base_branch":"","workspace":""}
  ```
  `title` は必須。`workspace` は local の作業先（任意。空ならタイトル／説明の絶対パスを自動抽出）。
  サーバーは `CreateTask` して mgr に渡す。TUI からは `/task -d <作業先> <タイトル>` で指定できる。

### 15.2 TUI の操作

- 下部に入力行。`Enter` で送信。
- スラッシュコマンド: `/task <タイトル>` でタスク投入、`/help` でヘルプ、`/quit` で終了。
- `@mgr` / `@dev_m` / `@dev_f` / `@chat` / `@all` を本文先頭に書くと、その社員が応答する。
- スクロール: `PageUp` / `PageDown` / `Home` / `End`。
- 終了は `Esc` または `Ctrl-C`（`q` は入力文字として扱う）。

---

## 16. Phase 5（経済・社会）

### 16.1 #会議室 の @宛先（§15 の拡張）

- `@mgr 本文` / `@dev_m 本文` / `@dev_f 本文` / `@chat 本文` … その社員が 1 往復で応答する。
- `@all 本文` … 全員が応答する（点呼など）。
- 宛先なし … chat 役が応答する（従来どおり）。
- 宛先は本文先頭の `@id`。全角の「＠」・全角スペースや `:`・`：`・`,`・`、` 区切りも正規化して
  受け付ける（`＠mgr　点呼` / `@mgr、点呼` / `@mgr: 点呼` はいずれも mgr 宛て）。
- 実装: `agents.Converser`（mgr / dev / chat が実装）を api に登録し、`say` の本文先頭の `@id` で振り分ける。

### 16.2 経済 API

- `GET  /api/ledger` 全社員の残高
- `GET  /api/ledger/:id/entries?limit=` 元帳履歴
- `GET  /api/economy/status` 残高分布（min/max/spread/alert）
- 格差観察 cron（`OFFICE_INEQUALITY_CRON`）: spread が `OFFICE_INEQUALITY_THRESHOLD` 以上なら通知する
  （**観察のみ**で行動はしない。労働運動トリガーはこの観察から将来実装）。

### 16.3 実務向けの既定

- worker の `exec` は `OFFICE_ALLOW_EXEC=1` で許可（`.env` で既定有効にした）。サンドボックスは
  `OFFICE_SANDBOX`（`bwrap` / `none`）。
- 接続時の履歴には system の入退室通知を含めない（再起動後の見づらさ対策）。
- dev エージェントの起動時 `【出勤】` 挨拶は削除（worker の check-in 通知と重複するため）。

### 16.4 実装で確定した裁量（§0 の方針）

- Phase 5.2「高級モデル購入（実行時モデル差し替え）」は全面廃止した。DeepSeek の V4 Flash / V4 Pro
  が V4.1 Flash（`deepseek-flash`）に統合され、上位モデルという概念が実質無くなったため（元帳・残高・給与・格差観察は存続）。
- TUI にヘッドレス発言（`--say TEXT`。複数回指定可、`--snapshot` と併用可）を追加した。
  E2E/自動化から `@宛先` を叩くため（`client/crates/tui/src/main.rs`）。
- `GET /api/ledger` は台帳に行が無い社員（残高 0。例えば日当制の chat）も含めて
  全社員を返す。`GET /api/economy/status` も同じ全社員を母集団にし（min/spread に 0 を含む）、
  `/api/ledger` と対象がずれないようにした。`GET /api/ledger/:id/entries` の `limit` は 1..200 に収め、
  存在しない社員は `/api/ledger/:id` ともども 404 を返す。
- `@宛先` は大文字小文字を区別しない（`@MGR` も `@mgr` と同じ）。未知の宛先
  （`@nobody`）は黙殺せず #会議室 に system 通知（`【宛先不明】@nobody という社員はいません`）を出す。
  `@` の直後が空白のとき（`@ 本文`）はメンション無し扱いにする。
- TUI の表示整理: 自分の発言はサーバーから届く `notice` のみで 1 回表示する
  （送信時のローカルエコーを廃止）。再接続時は `welcome` 受信で会話履歴をクリアし、
  履歴再送による二重表示を防ぐ。
- presence に「サーバー常駐社員（`mgr` / `chat`）＝常時在席・Expire 対象外」を追加した。両者は
  サーバー側の goroutine で動き worker 接続を持たないため、従来は heartbeat が無いまま
  `OFFICE_HEARTBEAT_TIMEOUT` で退勤表示になり、在席の見た目が実態と食い違っていた。
  `Registry.MarkResident` で起動時に在席登録し、`Expire` の対象から外すことで解消した。
  `dev_m` / `dev_f` は従来どおり worker の接続で出勤、切断・timeout で退勤する。
- `parseMention` は全角の「＠」と全角スペース（`\u3000`）を半角へ正規化し、宛先 ID（英数字と
  アンダースコア）の直後の区切りとして空白・`:`・`：`・`,`・`、` を読み飛ばすようにした。
  日本語 IME で全角記号が混入しやすく、`＠mgr　点呼` のような入力が宛先として解釈されず
  「chat しか応答しない」ように見える問題への対策（§16.1）。
- mgr の割当は「オンラインの dev を優先（候補を在席中に限定）→ 計画の指名 → 負荷分散」の
  順になった（`Manager.nextAssignee`）。worker 未接続の dev に割り当てて失敗するのを減らす
  ため（§16.5）。
- TUI の入力ルーティング: 保留中の質問（`回答> `）があっても、`@` または `/` で始まる入力は
  回答に飲み込まず通常会議室発言として送る。`@宛先` やコマンドが回答扱いになり
  「chat しか応答しない」ように見える問題を避けるためで、保留は解除せずそのまま残す。
- タスクごとの作業先（`workspace`）を指定できるようにした。`task_assign` payload /
  `POST /api/tasks` の `workspace`、TUI の `/task -d <作業先>` で渡す。許可範囲は
  `OFFICE_ALLOWED_ROOTS`（`:` 区切り）で、`OFFICE_WORKSPACE` 自体も常に含める。サーバーは
  `OFFICE_ALLOWED_ROOTS` 設定時に事前チェックし、worker が canonicalize 後に前方一致で最終検証する。
- タイトル／説明に絶対パスが含まれるとき、それを作業先として自動抽出する（`extractWorkspacePath`。
  URL の `://` は誤検出しない。例: `/home/u/Dev/siteに、天気...` → `/home/u/Dev/site`）。
- 同一社員 ID の重複接続は、新しい接続で古い接続を閉じて置き換える（`hub.evictEmployee`）。
  置き換えで閉じた旧接続は、同社員の別接続がまだ在席なら退勤処理をスキップし、誤った退勤にしない。
- `exec` の改善: `args` が空で `cmd` がシェル風の 1 行コマンド（空白や `&&` 等のメタ文字を含む）
  のときは `sh -c` で実行する（LLM がその形式を出しやすいため）。`OFFICE_ALLOW_EXEC=1` が
  必要な点は変わらない。

### 16.5 質問と回答（エスカレーション）

担当者（dev）が作業中に情報不足で判断できない場合、mgr 経由でオーナーへ質問を上げ、回答を
受け取ってから作業をやり直す（§0 の裁量拡張。§4.2/§15 の「新しい type の追加は自由」に基づく）。

流れ:

```
dev --Escalate(Question)--> mgr --AskOwner--> api（保存 + question 配信）
オーナー --answer--> api --AnswerQuestion--> mgr --Answer--> Escalate の戻り値 --> dev
```

wire（§4.2 / §15 の type 拡張）:

- Server → Client `question`:
  ```json
  {"type":"question","id":"uuid","from":"dev_m","text":"...","task_id":"...","ts":"..."}
  ```
- Client → Server `answer`:
  ```json
  {"type":"answer","question_id":"uuid","text":"..."}
  ```

実装:

- `agents.Question`（ID / FromID / TaskID / Text）。`agents.Escalator`（`Escalate(ctx, Question) (string, bool)`。
  `Manager` が実装）。`agents.OwnerChannel`（`AskOwner(ctx, Question) error`。api が実装）。
- mgr 側: `Manager.Escalate` が質問 ID を採番し、回答受け渡しチャネルを登録して `AskOwner` で配信、
  回答またはタイムアウトまで待つ。`Manager.Answer` が `answer` を待機中の `Escalate` へ渡す。
  「管理職を通す」ため、質問は mgr 名義で `#会議室` に保存・配信する。
- TUI: `question` を受けると `#会議室` に `dev_m（質問）: ...` と表示し、未回答の間は入力プロンプトを
  `回答> ` にする（`Enter` で回答送信。`/say` は通常発言、`/skip`・`/cancel` は保留解除）。
- 上限: `OFFICE_ANSWER_TIMEOUT`（既定 `3m`）。回答が得られない場合は ok=false で、担当者は
  フォールバックして作業を続ける。

方針:

- オーナー（TUI 等）が未接続のときは質問せず即フォールバックする（`AskOwner` がエラーを返す）。
  長時間ブロックしてタスクが滞留するのを避けるため。
- mgr の割当は、オンライン（在席中）の dev を優先して候補を絞り、計画文で担当（`dev_m` /
  `dev_f`）を名指ししていればそれを選び、無ければこれまでの担当件数が少ない方へ割り当てる
  （`Manager.nextAssignee`。旧ラウンドロビン固定を置換）。worker 未接続の dev に割り当てて
  失敗するのを減らすため、オンライン優先を先に見る（presence 未登録なら従来どおり全員を候補にする）。

### 16.6 自律実行と自発活動

§0 の裁量拡張として、エージェントの自律実行（dev の反復ループ）と自発活動（自律発言・AI 同士の交流）を追加した。

- **dev の反復ループ**: `DevAgent.work` の local 実行を、従来の「1 回計画 → 1 回実行」から
  `DevAgent.localLoop`（`planStep` / `stepPrompt`）に置き換えた。「次の一手」を LLM に 1 回問い合わせ、
  返ってきた JSON を解釈して worker で実行し、その結果（observation）を次の問い合わせに渡す、を繰り返す。
  - JSON 契約: `{"actions":[{"op":"write|read|list|exec",...}]}`（作業を進める）/
    `{"done":true,"summary":"..."}`（完了）/ `{"question":"..."}`（情報不足。mgr 経由でオーナーへ
    エスカレーションし、回答を observation に足して続行。§16.5）。
  - 上限: `OFFICE_MAX_AGENT_TURNS`（`MaxTurns`、既定 8）と `OFFICE_TOKEN_BUDGET`（`TokenBudget`、既定 20000）。
    `{"actions"}` も `{"question"}` も無い応答や上限到達では、直前の要約で `done` として終了する。
  - 初回の応答が解釈不能（`planStep` が空）のときは、従来の単発実行（`buildAssign` → `execute`）に
    フォールバックする（エージェント形式を返さないモデル向けの後方互換）。
  - remote モードは従来どおり単発（反復しない）。
- **worker の非致命アクション失敗**: local 実行（`client/crates/worker/src/local.rs`）で、個々の
  アクションの失敗（例: 存在しないファイルの `read`）は全体を止めず `WARN` として記録し、最後まで
  実行して集約する。1 件も成功しなかった場合のみ `failed` を返す（探索中の `read` 失敗を許容するため）。
- **自発活動**: `OFFICE_INITIATIVE_CRON`（既定 `*/10 * * * *`、`off` で無効）の cron から
  `api.Server.RunInitiative` が呼ばれ、各エージェントに `officeBrief`（在席・直近の発言・最近のタスク）を
  渡して自発的な発言を 1 つ生成させる。生成は `agents.InitiativeAgent`（`Initiative(ctx, brief) string`）を
  実装したエージェントが行い、空文字なら何も発言しない。
  - 生成された発言は `#会議室` へ投稿し、本文先頭が `@id` のときはその社員へ会話を 1 往復だけ振る
    （`routeAgentMention`）。相手の返答は通常の `Converse`（1 往復）で、そこからさらに連鎖しない
    （**深さ 1**）。`@all` は発言者以外の全員へ振る。
  - これにより、オーナーが指示しなくても進捗共有や声かけが自発的に行われる。

### 16.7 全員で分担（プロジェクト管理）

§0 の裁量拡張として、mgr にプロジェクト管理・社員管理の役割を持たせ、1 つのタスクを複数の dev に
分担させる機構を追加した。

- **mgr の役割**: プロジェクト（親タスク）の分解・割当・完了集約を担う。割当は**オンライン優先・負荷分散**
  （`Manager.nextAssignee`。在席中の dev を優先し、計画の指名が無ければ負荷の少ない方へ割り当てる）。
- **計画 JSON 契約**: mgr の計画フェーズ（`planningInstruction`）で、分担が必要なときは次の JSON だけを
  返させる。分担が不要なら従来どおり日本語の計画文を返し、1 人の dev に割り当てる。
  ```json
  {"summary":"計画の要約","subtasks":[{"title":"作業名","detail":"作業内容","assignee":"dev_m または dev_f"}]}
  ```
- **実装（mgr）**: `Manager.handleTask` が `parseSubtasks` で計画文から `summary` / `subtasks` を取り出し
  （JSON でなければ従来の単発割当へ）、分担ありなら `Manager.dispatchSubtasks` で各サブタスクを切り、
  `Manager.Review` が子タスクの結果を受けて `Manager.aggregateProject` で親を集約する。
  - `parseSubtasks(text)`: `summary` / `subtasks` を返す。JSON が無い・`title` が空・`subtasks` が空なら ok=false。
  - `dispatchSubtasks`: 親を `working`（`N 件に分担中`）にし、各サブタスクを `nextAssignee` で選んだ dev に
    割り当てて `CreateSubtask` → 配送する。担当範囲を明示する一文
    （「このサブタスクの範囲だけを担当し、他の担当者のファイルは作成しないでください。」）を `detail` に足して
    重複を抑える。1 件も割当てられなければ親を `failed` にする。
  - `aggregateProject`: 子の状態を集計し、全 `done` → 親 `done`（`全 N サブタスク完了`）、
    1 つでも `failed` → 親 `failed`（要対応を通知）、それ以外は進捗ログのみ。
- **実装（api）**: `agents.TaskCoordinator`（`CreateSubtask` / `SubtaskStatuses` / `ParentTaskID`）を
  api が実装する（`server/internal/api/coordinator.go`）。`CreateSubtask` は親の `Mode` / `Repo` / `BaseBranch`
  を引き継ぎ、`status=assigned` で子タスクを作成してその ID を返す（配送・割当は mgr が行う）。
  `SubtaskStatuses` は `store.Subtasks(parentID)` を、`ParentTaskID` は `store.Task(taskID)` の `ParentID` を返す。
- **データモデル**: `tasks.parent_id` を追加した（migration v3。`ALTER TABLE tasks ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`
  と `idx_tasks_parent`）。設計書 §5「カラム追加はOK」に基づく追補。親タスクは `parent_id=''`、子は親 ID を持つ。
- **共有作業先の注意**: 同じプロジェクトを作るには**全 dev が同じ作業先を使う**必要がある。オーナーは
  `/task -d <プロジェクトdir> <タイトル>`（またはタイトル中の絶対パス）で作業先を指定し、親タスクの
  `Workspace` はサブタスクにも引き継がれる（`dispatchSubtasks` が親の `Task` を複製して `ID` / `Title` /
  `Description` を差し替えるため）。
