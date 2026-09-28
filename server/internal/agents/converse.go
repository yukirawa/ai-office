package agents

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
)

// Converser は #会議室 などでの 1 往復の会話に応答できる（mgr / dev / chat）。
type Converser interface {
	Converse(ctx context.Context, channel, prompt string)
}

// Agent は mgr / dev / chat の共通インターフェース。
type Agent interface {
	Converser
}

// InitiativeAgent は自発的な発言を生成できる（§16 自律）。
// 返したテキストの投稿・宛先振り分けは呼び出し側（api）が行う。
type InitiativeAgent interface {
	// Initiative はオフィスの状況 brief から、自発的な発言（無ければ空文字）を返す。
	Initiative(ctx context.Context, brief string) string
}

// 3 種のエージェントが Agent を満たすことをコンパイル時に保証する。
var (
	_ Agent = (*Manager)(nil)
	_ Agent = (*DevAgent)(nil)
	_ Agent = (*ChatAgent)(nil)
)

// defaultConverseTimeout は Converse 1 回の上限。opts.TaskTimeout が未設定のときに使う。
// LLM 呼び出しがハングしてもここで打ち切る（§6.3 の無限ループ防止の一環）。
const defaultConverseTimeout = 60 * time.Second

// converseFallbackReply は応答テキストが空だったときにチャンネルへ流す文言。
const converseFallbackReply = "(うまく言葉が出てきませんでした…)"

// converseInstruction は Converser 共通の追加システムプロンプトを組み立てる。
func converseInstruction(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "AI 社員"
	}
	return fmt.Sprintf(
		"あなたは「%s」です。#会議室 での発言に、日本語で 1〜2 文の短い返事をしてください。自分の担当（管理職/開発/雑談）を踏まえて簡潔に。",
		name,
	)
}

// buildConverseSystemPrompt はペルソナのシステムプロンプトに会話用の指示を足して返す。
func buildConverseSystemPrompt(base, name string) string {
	base = strings.TrimSpace(base)
	instr := converseInstruction(name)
	if base == "" {
		return instr
	}
	return base + "\n\n" + instr
}

// initiativeInstruction は自発発言用の追加システムプロンプト（§16 自律）。
func initiativeInstruction(name, role string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "AI 社員"
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "AI 社員"
	}
	return fmt.Sprintf(
		"あなたは「%s」（%s）です。オフィスの状況を踏まえ、自発的に次にすること 1 つを決めてください。"+
			"発言するときは #会議室 へそのまま流す日本語の短い文（1〜2 文）だけを返してください。"+
			"誰かに依頼・確認・雑談を振るときは本文の先頭に @mgr / @dev_m / @dev_f / @chat のいずれかを付けます。"+
			"特にすることが無ければ空文字だけを返してください。JSON や説明は不要です。",
		name, role)
}

// initiativeSystemPrompt はペルソナのシステムプロンプトに自発発言の指示を足して返す。
func initiativeSystemPrompt(base, name, role string) string {
	base = strings.TrimSpace(base)
	instr := initiativeInstruction(name, role)
	if base == "" {
		return instr
	}
	return base + "\n\n" + instr
}

// initiativeRun は自発発言の LLM 呼び出しを 1 回行い、整形したテキストを返す。
func initiativeRun(ctx context.Context, client llm.Client, logger *slog.Logger, id, model, system, brief string, opts Options) (string, bool) {
	text, ok := converseRun(ctx, client, logger, id, model, system, brief, opts)
	if !ok {
		return "", false
	}
	return normalizeInitiative(sanitizeChatReply(text)), true
}

// normalizeInitiative は「特に無し」を表す返答を空文字に正規化する。
func normalizeInitiative(text string) string {
	t := strings.TrimSpace(text)
	switch t {
	case "", "なし", "特になし", "無し", "何もしない", "none", "None", "N/A", "-":
		return ""
	}
	return t
}

// converseTimeout は Converse 1 回の上限時間を決める。
// TaskTimeout が正ならそれを、そうでなければ既定 60s を使う。
func converseTimeout(opts Options) time.Duration {
	if opts.TaskTimeout > 0 {
		return opts.TaskTimeout
	}
	return defaultConverseTimeout
}

// converseRun は会話 1 往復の LLM ループを実行し、最終テキストと投稿可否を返す。
// 無限ループ防止（最大ターン数・トークン予算）と終端判定を mgr / dev / chat で共有する（§6.3）。
//
// client が nil・LLM エラー・ctx 終了の場合は ok=false（呼び出し側は投稿しない）。
// LLM 実装側の panic を握りつぶして ok=false にし、Run ループを守る。
func converseRun(ctx context.Context, client llm.Client, logger *slog.Logger, id, model, system, prompt string, opts Options) (lastText string, ok bool) {
	if client == nil {
		return "", false
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("会話応答の処理中に panic しました", "id", id, "panic", r)
			lastText, ok = "", false
		}
	}()

	req := llm.Request{
		Model:     model,
		System:    system,
		Messages:  []llm.Message{{Role: "user", Content: chatPrompt(prompt)}},
		MaxTokens: opts.MaxTokens,
	}

	maxTurns := opts.MaxTurns
	budget := opts.TokenBudget

	tokensUsed := 0
	guard := ""
	turns := 0

	for turn := 1; turn <= maxTurns; turn++ {
		turns = turn

		// ctx が終わったら即座に抜ける（呼び出し元の終了処理を妨げない）。
		if err := ctx.Err(); err != nil {
			logger.Info("コンテキスト終了のため会話応答を中断します", "id", id, "error", err)
			return "", false
		}

		resp, err := client.Chat(ctx, req)
		if err != nil {
			// エラーは記録して投稿しない（Run は落とさない）。
			logger.Error("LLM 呼び出しに失敗しました", "id", id, "turn", turn, "error", err)
			return "", false
		}

		tokensUsed += resp.InputTokens + resp.OutputTokens
		lastText = resp.Text

		// ガード1: トークン予算超過。
		if tokensUsed > budget {
			guard = "token_budget"
			break
		}
		// 終端理由なら 1 ターンで完了（会話は通常ここで終わる）。
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
		logger.Warn("会話の無限ループ防止ガードが作動しました",
			"id", id,
			"guard", guard,
			"turns", turns,
			"tokens_used", tokensUsed,
			"max_turns", maxTurns,
			"token_budget", budget,
		)
	}
	return lastText, true
}
