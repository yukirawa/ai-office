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

	// models は実行時に差し替え可能なモデル名を保持する（Phase 5 の高級モデル購入）。
	models modelState
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
		models:   newModelState(opts.Model),
	}
}

// ID は社員 ID を返す。
func (c *ChatAgent) ID() string { return c.id }

// SetModel は実行時に使うモデルを差し替える（Modeler、Phase 5）。
// 空文字は設定既定（Options.Model、無ければ DefaultModel）へ戻す。
func (c *ChatAgent) SetModel(model string) { c.models.set(model) }

// Model は現在使うモデル名を返す（スレッドセーフ）。
func (c *ChatAgent) Model() string { return c.models.get() }

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

// handlePrompt は 1 件の入力に LLM で応答してチャンネルへ投稿する（Run からの呼び出し）。
func (c *ChatAgent) handlePrompt(ctx context.Context, prompt string) {
	c.respond(ctx, c.opts.Channel, prompt)
}

// Converse は #会議室 などでの 1 往復の会話に応答する（Converser）。
// メッセージ受信箱とは独立に、その場で LLM を呼んで指定チャンネルへ投稿する。
func (c *ChatAgent) Converse(ctx context.Context, channel, prompt string) {
	if strings.TrimSpace(channel) == "" {
		channel = c.opts.Channel
	}
	ctx, cancel := context.WithTimeout(ctx, converseTimeout(c.opts))
	defer cancel()
	c.respond(ctx, channel, prompt)
}

// respond はチャンネルへ 1 往復の会話応答を返す。handlePrompt と Converse が共有する。
// 無限ループ防止（最大ターン数・トークン予算）と終端判定は converseRun に集約している（§6.3）。
func (c *ChatAgent) respond(ctx context.Context, channel, prompt string) {
	c.setState(StateThinking)

	if c.client == nil {
		c.logger().Error("LLM クライアントが未設定のため応答できません", "id", c.id)
		c.setState(StateIdle)
		return
	}

	text, ok := converseRun(ctx, c.client, c.logger(), c.id, c.Model(), c.systemPrompt(), prompt, c.opts)
	if !ok {
		c.setState(StateIdle)
		return
	}

	reply := sanitizeChatReply(text)
	if reply == "" {
		reply = converseFallbackReply
	}
	c.notifyChannel(ctx, channel, reply)
	c.setState(StateIdle)
}

// systemPrompt はペルソナのシステムプロンプトに会話用の指示を足して返す。
func (c *ChatAgent) systemPrompt() string {
	return buildConverseSystemPrompt(c.persona.SystemPrompt(), c.name)
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

// notify は既定チャンネル（Options.Channel）へ投稿する。
func (c *ChatAgent) notify(ctx context.Context, text string) {
	c.notifyChannel(ctx, c.opts.Channel, text)
}

// notifyChannel は指定チャンネルへ投稿する。エラーはログに記録して握りつぶす。
func (c *ChatAgent) notifyChannel(ctx context.Context, channel, text string) {
	if c.notifier == nil {
		return
	}
	if err := c.notifier.Notify(ctx, channel, c.id, text); err != nil {
		c.logger().Error("通知の投稿に失敗しました", "channel", channel, "error", err)
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
