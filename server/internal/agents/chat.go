package agents

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// chatMaxReplyRunes は 1 応答としてチャンネルへ流す最大文字数（rune）。
const chatMaxReplyRunes = 500

// chatInstruction は雑談役に与える追加システムプロンプト（§8 4.3）。
const chatInstruction = "あなたは雑談・雑用担当です。日本語で 1〜2 文の短い返事だけをしてください。説明や箇条書きは不要です。"

// chatContinueInstruction は応答が終端でなかった場合に次のターンへ促す文言。
const chatContinueInstruction = "もっと短く、1〜2 文でまとめてください。"

// ChatAgent は雑談・雑用役（§8 4.3/4.4）。Ollama は使わずサーバーの llm.Client で応答する。
// ループ構造は mgr / dev と同じ Inbox → think → report → idle（§6.3）。
type ChatAgent struct {
	id       string
	name     string
	client   llm.Client
	notifier Notifier
	persona  persona.Persona
	opts     Options

	inbox chan string

	mu    sync.RWMutex
	state string
}

// NewChatAgent は ChatAgent を生成する。Run を別 goroutine で呼ぶまで入力は処理されない。
func NewChatAgent(id string, p persona.Persona, client llm.Client, notifier Notifier, opts Options) *ChatAgent {
	opts = normalizeOptions(opts)
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}
	return &ChatAgent{
		id:       id,
		name:     name,
		client:   client,
		notifier: notifier,
		persona:  p,
		opts:     opts,
		inbox:    make(chan string, inboxCapacity),
		state:    StateIdle,
	}
}

// ID は社員 ID を返す。
func (c *ChatAgent) ID() string { return c.id }

// State は現在の状態を返す（スレッドセーフ）。
func (c *ChatAgent) State() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Inbox はプロンプト（雑談のお題・オーナーの発言）を送るためのチャンネルを公開する。
func (c *ChatAgent) Inbox() chan<- string { return c.inbox }

// Post はプロンプトを受信箱にノンブロッキングで積む。満杯またはクローズ済みなら false。
func (c *ChatAgent) Post(prompt string) (ok bool) {
	// クローズ済みチャンネルへの送信は panic するため recover で false に落とす。
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case c.inbox <- prompt:
		return true
	default:
		return false
	}
}

// Run は Inbox を処理し続ける。ctx.Done か Inbox のクローズまで終了しない。
// LLM / Notifier のエラーはログに記録するだけで、Run を落とさない。
func (c *ChatAgent) Run(ctx context.Context) {
	c.setState(StateIdle)
	c.logger().Info("chat エージェントを起動しました", "id", c.id, "name", c.name)
	c.notify(ctx, fmt.Sprintf("【出勤】%s (%s) が雑談係として入りました。話しかけてください。", c.name, c.id))

	for {
		select {
		case <-ctx.Done():
			c.logger().Info("コンテキスト終了のため chat エージェントを停止します", "id", c.id)
			return
		case prompt, ok := <-c.inbox:
			if !ok {
				c.logger().Info("受信箱が閉じられたため chat エージェントを停止します", "id", c.id)
				return
			}
			c.handlePrompt(ctx, prompt)
		}
	}
}

// handlePrompt は 1 件の入力に LLM で応答してチャンネルへ投稿する。
// 無限ループ防止（最大ターン数・トークン予算）を mgr / dev と同様に適用する（§6.3）。
func (c *ChatAgent) handlePrompt(ctx context.Context, prompt string) {
	c.setState(StateThinking)

	if c.client == nil {
		c.logger().Error("LLM クライアントが未設定のため応答できません", "id", c.id)
		c.setState(StateIdle)
		return
	}

	req := llm.Request{
		Model:    DefaultModel,
		System:   c.systemPrompt(),
		Messages: []llm.Message{{Role: "user", Content: chatPrompt(prompt)}},
	}

	maxTurns := c.opts.MaxTurns
	budget := c.opts.TokenBudget

	tokensUsed := 0
	var lastText string
	guard := ""
	turns := 0

	for turn := 1; turn <= maxTurns; turn++ {
		turns = turn

		// ctx が終わったら即座に抜ける（Run の終了処理を妨げない）。
		if err := ctx.Err(); err != nil {
			c.logger().Info("コンテキスト終了のため応答を中断します", "id", c.id, "error", err)
			c.setState(StateIdle)
			return
		}

		resp, err := c.client.Chat(ctx, req)
		if err != nil {
			// エラーは記録して次の入力を待つ（Run は落とさない）。
			c.logger().Error("LLM 呼び出しに失敗しました", "id", c.id, "turn", turn, "error", err)
			c.setState(StateIdle)
			return
		}

		tokensUsed += resp.InputTokens + resp.OutputTokens
		lastText = resp.Text

		// ガード1: トークン予算超過。
		if tokensUsed > budget {
			guard = "token_budget"
			break
		}
		// 終端理由なら 1 ターンで完了（雑談は通常ここで終わる）。
		if isTerminalStop(resp.StopReason) {
			break
		}
		// ガード2: 最大ターン数。
		if turn == maxTurns {
			guard = "max_turns"
			break
		}

		// まだ続きが必要な応答（max_tokens 等）は会話を積んで次ターンへ。
		req.Messages = append(req.Messages,
			llm.Message{Role: "assistant", Content: lastText},
			llm.Message{Role: "user", Content: chatContinueInstruction},
		)
	}

	if guard != "" {
		c.logger().Warn("雑談の無限ループ防止ガードが作動しました",
			"id", c.id,
			"guard", guard,
			"turns", turns,
			"tokens_used", tokensUsed,
			"max_turns", maxTurns,
			"token_budget", budget,
		)
	}

	reply := sanitizeChatReply(lastText)
	if reply == "" {
		reply = "(うまく言葉が出てきませんでした…)"
	}
	c.notify(ctx, reply)
	c.setState(StateIdle)
}

// systemPrompt はペルソナのシステムプロンプトに雑談用の指示を足して返す。
func (c *ChatAgent) systemPrompt() string {
	base := strings.TrimSpace(c.persona.SystemPrompt())
	if base == "" {
		return chatInstruction
	}
	return base + "\n\n" + chatInstruction
}

// setState は内部状態を更新し、Notifier に公開する。
func (c *ChatAgent) setState(s string) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()

	if c.notifier != nil {
		c.notifier.SetAgentState(c.id, s)
	}
}

// notify はチャンネルへ投稿する。エラーはログに記録して握りつぶす。
func (c *ChatAgent) notify(ctx context.Context, text string) {
	if c.notifier == nil {
		return
	}
	if err := c.notifier.Notify(ctx, c.opts.Channel, c.id, text); err != nil {
		c.logger().Error("通知の投稿に失敗しました", "channel", c.opts.Channel, "error", err)
	}
}

func (c *ChatAgent) logger() *slog.Logger {
	return c.opts.Logger
}

// chatPrompt は入力をユーザーメッセージへ整形する。
// 空入力（cron からの呼びかけ等）には汎用のお題を補う。
func chatPrompt(prompt string) string {
	p := strings.TrimSpace(prompt)
	if p == "" {
		p = "（特に用事はありません。何か一言お願いします）"
	}
	return p
}

// sanitizeChatReply は応答を 1 行に整形し、長すぎる場合は切り詰める。
// チャンネル表示が崩れないよう改行・連続空白を 1 つの空白に潰す。
func sanitizeChatReply(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > chatMaxReplyRunes {
		return string(r[:chatMaxReplyRunes]) + "…"
	}
	return s
}
