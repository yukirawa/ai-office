package api

// mention.go は #会議室 での @宛先（@mgr / @dev_m / @chat / @all）を
// 各エージェントの Converser に振り分ける（§15 の say 拡張）。

import (
	"context"
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

// parseMention は本文先頭の "@id" を解釈する。
// 例: "@mgr 点呼" -> ("mgr", "点呼")。メンションが無ければ ("", 本文)。
func parseMention(text string) (target, body string) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") {
		return "", t
	}
	rest := t[1:]
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+1:])
	}
	return strings.TrimSpace(rest), ""
}

// dispatchMention は @宛先に応じてエージェントへ会話を投げる（ノンブロッキング）。
// target が空なら chat 役に渡す（メンション無しの既定挙動）。
func (s *Server) dispatchMention(channel, target, body string) {
	switch strings.ToLower(strings.TrimSpace(target)) {
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
		ag := s.employee(strings.TrimSpace(target))
		if ag == nil {
			return
		}
		prompt := body
		if prompt == "" {
			prompt = "呼びかけに一言返してください。"
		}
		s.converseAsync(ag, channel, prompt)
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
