package agents

import "context"

// Task は作業単位（Phase 2 で拡張、§6.2 / §13.2）。
type Task struct {
	ID          string
	Title       string
	Description string
	From        string
	Assignee    string // 実行する worker（dev）の ID。mgr が割当時に埋める（§8 4.2）。
	Mode        string // "local" (default) | "remote"
	Repo        string // remote モードの対象リポジトリ "owner/name"
	BaseBranch  string // remote モードのベースブランチ
	Plan        string // mgr が立てた計画（dev に渡る）
}

// Action は local モードで worker に実行させる操作（§13.2）。
type Action struct {
	Op      string   `json:"op"` // "read" | "write" | "list" | "exec"
	Path    string   `json:"path,omitempty"`
	Content string   `json:"content,omitempty"`
	Cmd     string   `json:"cmd,omitempty"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

// RemoteFile は remote モードで GitHub に提出する 1 ファイル。
type RemoteFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// RemoteSpec は remote モードの割当内容（§13.2）。
// Token はサーバー（api）が Dispatch 時に発行して埋めるため、ここでは設定しない。
type RemoteSpec struct {
	Repo       string       `json:"repo"`
	BaseBranch string       `json:"base_branch"`
	Branch     string       `json:"branch"`
	Title      string       `json:"title"`
	Body       string       `json:"body"`
	Files      []RemoteFile `json:"files"`
	Token      string       `json:"token,omitempty"`
}

// TaskAssign は task_assign の payload（§13.2）。
//
// TaskID は Dispatch / Await の相関に使う内部用フィールドで、payload には含めない
// （task_id は task_assign メッセージ直下のフィールドとして api が付与する）。
type TaskAssign struct {
	TaskID  string      `json:"-"`
	Mode    string      `json:"mode"`
	Title   string      `json:"title"`
	Reason  string      `json:"reason,omitempty"`
	Actions []Action    `json:"actions,omitempty"`
	Remote  *RemoteSpec `json:"remote,omitempty"`
}

// Result は worker の実行結果（§13.1 の task_result 相当）。
type Result struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"` // "done" | "failed"
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

// Dispatcher はワーカーへの割当と結果待ち（api が実装）。
type Dispatcher interface {
	Dispatch(ctx context.Context, employeeID string, assign TaskAssign) error
	Await(ctx context.Context, taskID string) (Result, error)
}

// TaskUpdater は tasks テーブルの状態更新（api が実装）。
type TaskUpdater interface {
	UpdateTask(ctx context.Context, taskID, status, result string) error
}

// RelationshipUpdater は関係値を更新する（persona.Service が実装）。
type RelationshipUpdater interface {
	Adjust(ctx context.Context, fromID, toID string, affinityDelta, trustDelta int) error
}

// RemoteExecutor はワーカー不在時にサーバー側で remote 実行する（api が実装。nil 可）。
type RemoteExecutor interface {
	ExecuteRemote(ctx context.Context, spec RemoteSpec) (Result, error)
}

// Reviewer は dev の結果を mgr が承認する（Manager が実装）。
type Reviewer interface {
	Review(ctx context.Context, t Task, res Result)
}

// Question は担当者（dev など）の疑問。mgr 経由でオーナーへ上げる（§16 のエスカレーション）。
type Question struct {
	ID     string // 質問 ID（mgr が採番する）
	FromID string // 質問した社員（例: dev_m）
	TaskID string // 関連タスク（任意）
	Text   string // 質問本文
}

// Escalator は担当者の疑問を mgr 経由でオーナーへ上げ、回答を待つ（Manager が実装）。
// 回答が得られない（オーナー未接続・タイムアウト・ctx 終了）場合は ok=false。
type Escalator interface {
	Escalate(ctx context.Context, q Question) (answer string, ok bool)
}

// OwnerChannel はオーナー（TUI）へ質問を送る境界（api が実装する任意実装）。
// Notifier とは別インターフェースにして、テスト用の Notifier 実装を壊さないようにする。
type OwnerChannel interface {
	AskOwner(ctx context.Context, q Question) error
}
