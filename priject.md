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
- 5.2 高級モデル購入
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
- 高級モデル購入の価格設定
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
     "actions":[{"op":"write","path":"reports/x.md","content":"..."}]
  }}
  ```
  `mode` は `local`（ファイル操作・コマンド） | `remote`（GitHub API）。
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
  {"type":"task","title":"...","description":"...","mode":"local","repo":"","base_branch":""}
  ```
  `title` は必須。サーバーは `CreateTask` して mgr に渡す。

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
- 実装: `agents.Converser`（mgr / dev / chat が実装）を api に登録し、`say` の本文先頭の `@id` で振り分ける。
  実行時モデル差し替え用に `agents.Modeler`（SetModel/Model）も実装。

### 16.2 経済 API

- `GET  /api/ledger` 全社員の残高
- `GET  /api/ledger/:id/entries?limit=` 元帳履歴
- `POST /api/economy/purchase {employee_id, model?}` 高級モデル購入（残高を消費し、その社員のモデルを実行時差し替え）
- `GET  /api/economy/status` 残高分布（min/max/spread/alert）
- 価格は `OFFICE_PREMIUM_MODEL_PRICE`、対象モデルは `OFFICE_PREMIUM_MODEL`。
- 格差観察 cron（`OFFICE_INEQUALITY_CRON`）: spread が `OFFICE_INEQUALITY_THRESHOLD` 以上なら通知する
  （**観察のみ**で行動はしない。労働運動トリガーはこの観察から将来実装）。

### 16.3 実務向けの既定

- worker の `exec` は `OFFICE_ALLOW_EXEC=1` で許可（`.env` で既定有効にした）。サンドボックスは
  `OFFICE_SANDBOX`（`bwrap` / `none`）。
- 接続時の履歴には system の入退室通知を含めない（再起動後の見づらさ対策）。
- dev エージェントの起動時 `【出勤】` 挨拶は削除（worker の check-in 通知と重複するため）。
