// Package agents は AI 社員を 1 体 1 goroutine + channel で動かす。
//
// 設計書 §6.3 に対応する。ループ構造は Inbox → think → work → report → idle。
// 無限ループ防止（最大ターン数・トークン予算・同一結論の繰り返し検出）を必須とする。
//
// Phase 1.3 では mgr エージェントの「計画のみ」を実装する。実際の作業
// （ファイル編集や GitHub 操作）は Phase 2 以降で work フェーズに足す。
package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// エージェントの状態。Notifier.SetAgentState に渡す値としても使う。
const (
	StateIdle     = "idle"
	StateThinking = "thinking"
	StateWorking  = "working"
)

const (
	// defaultMaxTurns は 1 タスクあたりの最大ターン数（§6.3 の無限ループ防止）。
	defaultMaxTurns = 8
	// defaultTokenBudget は 1 タスクあたりの入出力合計トークン上限（§6.3）。
	defaultTokenBudget = 20000
	// defaultMaxTokens は 1 リクエストあたりの最大出力トークンの既定値。
	defaultMaxTokens = 1024
	// defaultChannel は報告先の既定チャンネル。
	defaultChannel = "#会議室"
	// inboxCapacity は受信箱のバッファ容量。Post はノンブロッキングなので溢れたら false。
	inboxCapacity = 16
	// repeatConclusionLimit は同一結論とみなすまでに許容する同一テキストの出現回数。
	repeatConclusionLimit = 3
)

// DefaultModel は Options.Model が未設定のときに使う既定モデル。
// api 側で OFFICE_LLM_MODEL から Options.Model へ注入できる。
//
// TODO(§11): モデル選定は未決定。既定値は暫定。
const DefaultModel = "claude-3-5-haiku-latest"

// レビュー結果に応じた関係値（affinity/trust）の増減量。
//
// TODO(§11): 値は暫定。運用しながら調整する（成功で緩やかに上げ、失敗で強めに下げる）。
const (
	// 成功: mgr -> assignee。
	relSuccessMgrToAssigneeAffinity = 2
	relSuccessMgrToAssigneeTrust    = 1
	// 成功: assignee -> mgr。
	relSuccessAssigneeToMgrAffinity = 1
	relSuccessAssigneeToMgrTrust    = 0
	// 失敗: mgr -> assignee。
	relFailureMgrToAssigneeAffinity = -3
	relFailureMgrToAssigneeTrust    = -2
)

// planningInstruction は mgr の計画フェーズ用の追加システムプロンプト。
const planningInstruction = "あなたは管理職です。タスクの実行計画を短く日本語で述べてください。実行はしないこと。"

// continueInstruction は応答が終端でなかった場合に次のターンへ促す文言。
const continueInstruction = "前回の計画を踏まえて更新してください。変化がなければ同じ内容を返して構いません。"

// Notifier は agent からチャンネル投稿・状態公開を行うための境界。
// api パッケージを import すると循環参照になるため、インターフェースで分離する。
type Notifier interface {
	Notify(ctx context.Context, channel, fromID, text string) error
	SetAgentState(employeeID, state string)
}

// Employee はエージェントの公開情報。
type Employee struct {
	ID      string
	Name    string
	Persona persona.Persona
	Inbox   chan Task
	State   string // "idle", "thinking", "working" など
}

// Options は Manager / DevAgent の挙動を調整する。ゼロ値は既定値に正規化される。
type Options struct {
	MaxTurns    int           // 1 タスクあたりの最大ターン数。既定 8
	TokenBudget int           // 1 タスクあたりの入出力合計トークン上限。既定 20000
	Model       string        // LLM モデル名。空なら DefaultModel
	MaxTokens   int           // 1 リクエストの最大出力トークン。0 以下なら既定 1024
	Channel     string        // 報告先チャンネル。既定 "#会議室"
	TaskTimeout time.Duration // worker の実行結果を待つ上限。既定 5 分
	Logger      *slog.Logger  // 既定 slog.Default()
}

// Manager は 1 体の AI 社員を動かす。1 Manager = 1 goroutine を想定する。
type Manager struct {
	client   llm.Client
	notifier Notifier
	opts     Options

	employee Employee

	// 割当先（dev）とタスク状態更新・関係値更新は構築後に差し替えられるため、
	// 専用のミューテックスで保護する（§13.3 の状態遷移を mgr が駆動する）。
	assignMu  sync.Mutex
	assignees []*DevAgent
	assignIdx int
	updater   TaskUpdater
	rel       RelationshipUpdater

	mu    sync.RWMutex
	state string
}

// NewManager は Manager を生成する。Run を別 goroutine で呼ぶまでタスクは処理されない。
func NewManager(id string, p persona.Persona, client llm.Client, notifier Notifier, opts Options) *Manager {
	opts = normalizeOptions(opts)
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}
	return &Manager{
		client:   client,
		notifier: notifier,
		opts:     opts,
		employee: Employee{
			ID:      id,
			Name:    name,
			Persona: p,
			Inbox:   make(chan Task, inboxCapacity),
			State:   StateIdle,
		},
		state: StateIdle,
	}
}

// normalizeOptions はゼロ値を既定値で埋める。呼び出し側の Options は変更しない。
func normalizeOptions(opts Options) Options {
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = defaultMaxTurns
	}
	if opts.TokenBudget <= 0 {
		opts.TokenBudget = defaultTokenBudget
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = DefaultModel
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = defaultMaxTokens
	}
	if strings.TrimSpace(opts.Channel) == "" {
		opts.Channel = defaultChannel
	}
	if opts.TaskTimeout <= 0 {
		opts.TaskTimeout = defaultTaskTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return opts
}

// SetAssignees は dev エージェントの割当先を設定する。構築後でも呼べる。
// 以降のタスクはラウンドロビンで均等に割り当てられる。
func (m *Manager) SetAssignees(assignees ...*DevAgent) {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	m.assignees = append([]*DevAgent(nil), assignees...)
	m.assignIdx = 0
}

// SetTaskUpdater はタスク状態を永続化する実装（api）を設定する。構築後でも呼べる。
func (m *Manager) SetTaskUpdater(u TaskUpdater) {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	m.updater = u
}

// SetRelationships は関係値の更新実装（persona.Service）を設定する。構築後でも呼べる。
func (m *Manager) SetRelationships(u RelationshipUpdater) {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	m.rel = u
}

// nextAssignee は次の割当先をラウンドロビンで返す。未設定なら nil。
func (m *Manager) nextAssignee() *DevAgent {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	if len(m.assignees) == 0 {
		return nil
	}
	a := m.assignees[m.assignIdx%len(m.assignees)]
	m.assignIdx++
	return a
}

// taskUpdater は現在の TaskUpdater を返す（未設定なら nil）。
func (m *Manager) taskUpdater() TaskUpdater {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	return m.updater
}

// relationshipUpdater は現在の RelationshipUpdater を返す（未設定なら nil）。
func (m *Manager) relationshipUpdater() RelationshipUpdater {
	m.assignMu.Lock()
	defer m.assignMu.Unlock()
	return m.rel
}

// updateTask はタスク状態を更新する。エラーはログに記録するだけで握りつぶす。
func (m *Manager) updateTask(ctx context.Context, taskID, status, result string) {
	u := m.taskUpdater()
	if u == nil {
		return
	}
	if err := u.UpdateTask(ctx, taskID, status, result); err != nil {
		m.logger().Error("タスク状態の更新に失敗しました", "task_id", taskID, "status", status, "error", err)
	}
}

// Run は Inbox を処理し続ける。ctx.Done か Inbox のクローズまで終了しない。
// LLM / Notifier のエラーはログに記録するだけで、Run を落とさない。
func (m *Manager) Run(ctx context.Context) {
	m.setState(StateIdle)
	m.logger().Info("エージェントを起動しました", "id", m.employee.ID, "name", m.employee.Name)

	// 起動時（タスク待ちの前）に出勤の挨拶を投稿する。
	m.notify(ctx, fmt.Sprintf("【出勤】%s (%s) が出勤しました。タスクをお待ちしています。", m.employee.Name, m.employee.ID))

	for {
		select {
		case <-ctx.Done():
			m.logger().Info("コンテキスト終了のためエージェントを停止します", "id", m.employee.ID)
			return
		case t, ok := <-m.employee.Inbox:
			if !ok {
				// 呼び出し側が Inbox をクローズした場合（明示的な停止要求）。
				m.logger().Info("受信箱が閉じられたためエージェントを停止します", "id", m.employee.ID)
				return
			}
			m.handleTask(ctx, t)
		}
	}
}

// Post はタスクを受信箱にノンブロッキングで積む。
// 満杯またはクローズ済みの場合は false を返す。
func (m *Manager) Post(t Task) (ok bool) {
	// クローズ済みチャンネルへの送信は panic するため、明示的な Close API が
	// 無い現状では recover で false に落とす（§6.3 の「満杯 or クローズで false」）。
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case m.employee.Inbox <- t:
		return true
	default:
		return false
	}
}

// State は現在の状態を返す（スレッドセーフ）。
func (m *Manager) State() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Inbox はタスクを送るためのチャンネルを公開する。
func (m *Manager) Inbox() chan<- Task {
	return m.employee.Inbox
}

// Employee はエージェントの公開情報を返す（読み取り専用のスナップショット）。
func (m *Manager) Employee() Employee {
	e := m.employee
	e.State = m.State()
	return e
}

// handleTask は 1 タスクを think → work → report → idle の順に処理する。
func (m *Manager) handleTask(ctx context.Context, t Task) {
	m.setState(StateThinking)

	if m.client == nil {
		m.logger().Error("LLM クライアントが未設定のためタスクを処理できません", "task_id", t.ID)
		m.setState(StateIdle)
		return
	}

	req := llm.Request{
		Model:     m.opts.Model,
		System:    m.systemPrompt(),
		Messages:  []llm.Message{{Role: "user", Content: taskPrompt(t)}},
		MaxTokens: m.opts.MaxTokens,
	}

	maxTurns := m.opts.MaxTurns
	budget := m.opts.TokenBudget

	tokensUsed := 0
	repeats := make(map[string]int, repeatConclusionLimit)
	var lastText string
	guard := ""
	turns := 0

	for turn := 1; turn <= maxTurns; turn++ {
		turns = turn

		// ctx が終わったら即座に抜ける（Run が終了処理に入るのを妨げない）。
		if err := ctx.Err(); err != nil {
			m.logger().Info("コンテキスト終了のためタスクを中断します", "task_id", t.ID, "error", err)
			m.setState(StateIdle)
			return
		}

		resp, err := m.client.Chat(ctx, req)
		if err != nil {
			// エラーは記録して次のタスクへ進む（Run は落とさない）。
			m.logger().Error("LLM 呼び出しに失敗しました", "task_id", t.ID, "turn", turn, "error", err)
			m.setState(StateIdle)
			return
		}

		tokensUsed += resp.InputTokens + resp.OutputTokens
		lastText = resp.Text
		m.setState(StateWorking)

		// ガード1: 同一結論の繰り返し検出。
		if text := strings.TrimSpace(lastText); text != "" {
			key := hashText(text)
			repeats[key]++
			if repeats[key] >= repeatConclusionLimit {
				guard = "repeated_conclusion"
				break
			}
		}

		// ガード2: トークン予算超過。
		if tokensUsed > budget {
			guard = "token_budget"
			break
		}

		// 終端理由なら 1 ターンで完了（Phase 1.3 の計画は通常ここで終わる）。
		if isTerminalStop(resp.StopReason) {
			break
		}

		// まだ続きが必要な応答（max_tokens / tool_use 等）は会話を積んで次ターンへ。
		req.Messages = append(req.Messages,
			llm.Message{Role: "assistant", Content: lastText},
			llm.Message{Role: "user", Content: continueInstruction},
		)

		// ガード3: 最大ターン数。
		if turn == maxTurns {
			guard = "max_turns"
		}
	}

	if guard != "" {
		m.logger().Warn("無限ループ防止ガードが作動しました",
			"task_id", t.ID,
			"guard", guard,
			"turns", turns,
			"tokens_used", tokensUsed,
			"max_turns", maxTurns,
			"token_budget", budget,
		)
	}

	plan := strings.TrimSpace(lastText)
	if plan == "" {
		plan = "(計画を生成できませんでした)"
	}
	m.notify(ctx, reportPrefix(t)+plan)

	// 計画を dev に割り当てる（§13.3: pending → assigned）。
	m.assignTask(ctx, t, plan)
	m.setState(StateIdle)
}

// taskAssigner は任意実装。tasks.assignee を更新できる場合に使う（api が実装）。
// 必須のインターフェースにしないのは、テスト用の TaskUpdater 実装を壊さないため。
type taskAssigner interface {
	AssignTask(ctx context.Context, taskID, assignee string) error
}

// assignTask は計画済みタスクを次の dev に割り当てる。
func (m *Manager) assignTask(ctx context.Context, t Task, plan string) {
	assignee := m.nextAssignee()
	if assignee == nil {
		m.notify(ctx, fmt.Sprintf("【%s】担当者が割り当てられていません。", taskLabel(t)))
		m.updateTask(ctx, t.ID, "failed", "no assignee")
		return
	}

	task := t
	task.Plan = plan
	task.Assignee = assignee.ID()
	// tasks.assignee を実行担当に更新する（任意実装のため型アサーションで呼ぶ）。
	if a, ok := m.taskUpdater().(taskAssigner); ok {
		if err := a.AssignTask(ctx, t.ID, assignee.ID()); err != nil {
			m.logger().Error("担当者の記録に失敗しました", "task_id", t.ID, "error", err)
		}
	}
	m.updateTask(ctx, t.ID, "assigned", plan)
	if !assignee.Post(task) {
		m.updateTask(ctx, t.ID, "failed", "assignee inbox full")
		m.notify(ctx, fmt.Sprintf("【%s】担当者の受信箱が満杯のため割り当てできませんでした。", taskLabel(t)))
		return
	}
	m.logger().Info("タスクを割り当てました", "task_id", t.ID, "assignee", assignee.ID())
}

// Review は dev の結果を承認し、タスクを done / failed へ遷移させる（§13.3）。
// Reviewer インターフェースを実装する。
func (m *Manager) Review(ctx context.Context, t Task, res Result) {
	status := "failed"
	label := "差し戻し"
	success := strings.TrimSpace(res.Status) == "done"
	if success {
		status = "done"
		label = "done"
	}

	// タスク状態を更新する前に、成功/失敗に応じて関係値を動かす（§8 4.2）。
	// ここでの失敗はログに記録するだけで、レビュー自体は続行する。
	m.adjustRelationships(ctx, t, success)

	m.updateTask(ctx, t.ID, status, res.Summary)
	m.notify(ctx, fmt.Sprintf("【レビュー】タスク「%s」を %s としました: %s", taskLabel(t), label, res.Summary))
	m.logger().Info("タスクをレビューしました", "task_id", t.ID, "status", status)
}

// adjustRelationships は Review の結果に応じて mgr と担当者の関係値を更新する。
// updater 未設定・担当者未設定の場合は何もしない。
func (m *Manager) adjustRelationships(ctx context.Context, t Task, success bool) {
	u := m.relationshipUpdater()
	if u == nil {
		return
	}
	assignee := strings.TrimSpace(t.Assignee)
	if assignee == "" {
		return
	}
	mgrID := m.employee.ID

	if success {
		m.adjustRelationship(ctx, u, t.ID, mgrID, assignee, relSuccessMgrToAssigneeAffinity, relSuccessMgrToAssigneeTrust)
		m.adjustRelationship(ctx, u, t.ID, assignee, mgrID, relSuccessAssigneeToMgrAffinity, relSuccessAssigneeToMgrTrust)
		return
	}
	m.adjustRelationship(ctx, u, t.ID, mgrID, assignee, relFailureMgrToAssigneeAffinity, relFailureMgrToAssigneeTrust)
}

// adjustRelationship は 1 方向の関係値を更新する。エラーはログのみで Review を止めない。
func (m *Manager) adjustRelationship(ctx context.Context, u RelationshipUpdater, taskID, fromID, toID string, affinityDelta, trustDelta int) {
	if err := u.Adjust(ctx, fromID, toID, affinityDelta, trustDelta); err != nil {
		m.logger().Error("関係値の更新に失敗しました",
			"task_id", taskID, "from", fromID, "to", toID, "error", err)
	}
}

// systemPrompt はペルソナのシステムプロンプトに計画用の指示を足して返す。
func (m *Manager) systemPrompt() string {
	base := strings.TrimSpace(m.employee.Persona.SystemPrompt())
	if base == "" {
		return planningInstruction
	}
	return base + "\n\n" + planningInstruction
}

// setState は内部状態を更新し、Notifier に公開する。
func (m *Manager) setState(s string) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()

	if m.notifier != nil {
		m.notifier.SetAgentState(m.employee.ID, s)
	}
}

// notify はチャンネルへ投稿する。エラーはログに記録して握りつぶす。
func (m *Manager) notify(ctx context.Context, text string) {
	if m.notifier == nil {
		return
	}
	if err := m.notifier.Notify(ctx, m.opts.Channel, m.employee.ID, text); err != nil {
		m.logger().Error("通知の投稿に失敗しました", "channel", m.opts.Channel, "error", err)
	}
}

func (m *Manager) logger() *slog.Logger {
	return m.opts.Logger
}

// taskPrompt はタスクを LLM に渡すユーザーメッセージに整形する。
func taskPrompt(t Task) string {
	var b strings.Builder
	b.WriteString("以下のタスクの実行計画を立ててください。\n")
	if t.ID != "" {
		fmt.Fprintf(&b, "タスクID: %s\n", t.ID)
	}
	if t.Title != "" {
		fmt.Fprintf(&b, "タイトル: %s\n", t.Title)
	}
	if t.From != "" {
		fmt.Fprintf(&b, "依頼者: %s\n", t.From)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "内容: %s\n", t.Description)
	}
	b.WriteString("\n実行はせず、計画のみを短く日本語で述べてください。")
	return b.String()
}

// taskLabel はタスクの表示名を返す（タイトルが無ければ ID で代替）。
func taskLabel(t Task) string {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = t.ID
	}
	if title == "" {
		title = "(無題タスク)"
	}
	return title
}

// reportPrefix は報告文の先頭に付けるタスク識別子を組み立てる。
func reportPrefix(t Task) string {
	from := strings.TrimSpace(t.From)
	if from == "" {
		from = "不明"
	}
	return fmt.Sprintf("【%s / 依頼: %s】", taskLabel(t), from)
}

// isTerminalStop は応答が「これ以上続ける必要がない」ことを示すか判定する。
// Anthropic の end_turn / stop_sequence、および stop_reason 未設定は終端扱い。
func isTerminalStop(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "", "end_turn", "stop_sequence":
		return true
	default:
		// max_tokens / tool_use / pause_turn などは継続余地ありとして扱う。
		return false
	}
}

// hashText は繰り返し検出用にテキストを短いハッシュへ変換する。
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
