package api

// mention.go は #会議室 での @宛先（@mgr / @dev_m / @chat / @all）を
// 各エージェントの Converser に振り分ける（§15 の say 拡張）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yukirawa/ai-office/server/internal/agents"
)

// conversationTimeout は 1 回の会話応答の上限。
const conversationTimeout = 90 * time.Second

// SetAgents は会話可能な社員エージェント（mgr / dev_m / dev_f / chat）を登録する。
func (s *Server) SetAgents(m map[string]agents.Agent) {
	s.mu.Lock()
	s.employees = m
	s.mu.Unlock()
}

// employee は登録済みエージェントを返す（未登録なら nil）。
func (s *Server) employee(id string) agents.Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.employees[id]
}

// mentionNormalizer は全角の「＠」と全角スペースを半角へ寄せる。
// 日本語 IME では全角の「＠」や「　」が混ざりやすく、そのままだと宛先として
// 解釈されず「chat しか応答しない」ように見えるため、先に正規化する。
var mentionNormalizer = strings.NewReplacer("＠", "@", "\u3000", " ")

// mentionSeparators は宛先 ID の直後に来る区切り文字として読み飛ばす文字集合。
const mentionSeparators = " \t\r\n:：,、"

// parseMention は本文先頭の "@id" を解釈する。
// id は英数字とアンダースコアの連続で、直後の区切り（空白・":"・"、"など）までを宛先、
// 残りを本文とする。全角の「＠」「　」は半角に正規化する。
// 例: "@mgr 点呼" -> ("mgr", "点呼") / "＠mgr　点呼" -> ("mgr", "点呼") /
//
//	"@mgr: 点呼" -> ("mgr", "点呼")。メンションが無ければ ("", 本文)。
func parseMention(text string) (target, body string) {
	t := strings.TrimSpace(mentionNormalizer.Replace(text))
	if !strings.HasPrefix(t, "@") {
		return "", t
	}
	rest := t[1:]
	if rest == "" {
		return "", ""
	}
	i := 0
	for i < len(rest) && isMentionIDChar(rest[i]) {
		i++
	}
	if i == 0 {
		// "@ だけ" のように @ の直後が ID でない場合はメンション無し扱いとし、本文全体を返す。
		return "", t
	}
	return rest[:i], strings.TrimSpace(strings.TrimLeft(rest[i:], mentionSeparators))
}

// isMentionIDChar は宛先 ID に使える ASCII 文字かを返す。
func isMentionIDChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return true
	default:
		return false
	}
}

// dispatchMention は @宛先に応じてエージェントへ会話を投げる（ノンブロッキング）。
// target が空なら chat 役に渡す（メンション無しの既定挙動）。
func (s *Server) dispatchMention(channel, target, body string) {
	// @mgr と @MGR が同じ結果になるよう、判定と lookup の双方で小文字化した id を使う。
	id := strings.ToLower(strings.TrimSpace(target))
	switch id {
	case "":
		if chat := s.chatAgent(); chat != nil {
			chat.Post(body)
		}
	case "all":
		prompt := body
		if prompt == "" {
			prompt = "点呼です。在席の一言をお願いします。"
		}
		s.forEachAgent(func(_ string, ag agents.Agent) { s.converseAsync(ag, channel, prompt) })
	default:
		ag := s.employee(id)
		if ag == nil {
			// 未知の宛先は黙って捨てず、#会議室 に system 通知で可視化する。
			// 通知文にはユーザー入力そのままの宛先（@ は付けない）を含める。
			s.notifyUnknownTarget(channel, strings.TrimSpace(target))
			return
		}
		prompt := body
		if prompt == "" {
			prompt = "呼びかけに一言返してください。"
		}
		s.converseAsync(ag, channel, prompt)
	}
}

// notifyUnknownTarget は解決できなかった @宛先を #会議室 に system 通知として投稿する。
// 送信者にエラーを返す経路が無いため、会話の可視フィードバックとして残す。
func (s *Server) notifyUnknownTarget(channel, target string) {
	text := fmt.Sprintf("【宛先不明】@%s という社員はいません", target)
	if err := s.Notify(context.Background(), channel, "system", text); err != nil {
		s.log.Warn("宛先不明の通知に失敗しました", "channel", channel, "target", target, "error", err)
	}
}

// converseAsync は 1 エージェントへ会話を投げる。LLM 待ちでブロックしないよう goroutine で実行する。
func (s *Server) converseAsync(ag agents.Agent, channel, prompt string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), conversationTimeout)
		defer cancel()
		ag.Converse(ctx, channel, prompt)
	}()
}

// forEachAgent は登録済みエージェントを走査する。
func (s *Server) forEachAgent(fn func(id string, ag agents.Agent)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, ag := range s.employees {
		fn(id, ag)
	}
}
