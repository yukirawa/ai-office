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
	// defaultChannel は報告先の既定チャンネル。
	defaultChannel = "#会議室"
	// inboxCapacity は受信箱のバッファ容量。Post はノンブロッキングなので溢れたら false。
	inboxCapacity = 16
	// repeatConclusionLimit は同一結論とみなすまでに許容する同一テキストの出現回数。
	repeatConclusionLimit = 3
)

// DefaultModel は Phase 1 で使う既定モデル。
// TODO(§11): モデル選定は未決定。設定（環境変数）から注入できるようにする。
const DefaultModel = "claude-3-5-haiku-latest"

// planningInstruction は mgr の計画フェーズ用の追加システムプロンプト。
const planningInstruction = "あなたは管理職です。タスクの実行計画を短く日本語で述べてください。実行はしないこと。"

// continueInstruction は応答が終端でなかった場合に次のターンへ促す文言。
const continueInstruction = "前回の計画を踏まえて更新してください。変化がなければ同じ内容を返して構いません。"

// Task はエージェントに割り当てられた作業単位。
type Task struct {
	ID          string
	Title       string
	Description string
	From        string
}

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

// Options は Manager の挙動を調整する。ゼロ値は既定値に正規化される。
type Options struct {
	MaxTurns    int          // 1 タスクあたりの最大ターン数。既定 8
	TokenBudget int          // 1 タスクあたりの入出力合計トークン上限。既定 20000
	Channel     string       // 報告先チャンネル。既定 "#会議室"
	Logger      *slog.Logger // 既定 slog.Default()
}

// Manager は 1 体の AI 社員を動かす。1 Manager = 1 goroutine を想定する。
type Manager struct {
	client   llm.Client
	notifier Notifier
	opts     Options

	employee Employee

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
	if strings.TrimSpace(opts.Channel) == "" {
		opts.Channel = defaultChannel
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return opts
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
		Model:    DefaultModel,
		System:   m.systemPrompt(),
		Messages: []llm.Message{{Role: "user", Content: taskPrompt(t)}},
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
	m.setState(StateIdle)
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

// reportPrefix は報告文の先頭に付けるタスク識別子を組み立てる。
func reportPrefix(t Task) string {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = t.ID
	}
	if title == "" {
		title = "(無題タスク)"
	}
	from := strings.TrimSpace(t.From)
	if from == "" {
		from = "不明"
	}
	return fmt.Sprintf("【%s / 依頼: %s】", title, from)
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
