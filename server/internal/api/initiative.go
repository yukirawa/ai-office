package api

// initiative.go は「自発活動」を実装する（§16 自律）。
//
// cron から RunInitiative が呼ばれ、各エージェントにオフィスの状況（brief）を渡して
// 自発的な発言を 1 つ生成させる。生成された発言は #会議室 へ投稿し、本文が @id で
// 始まる場合はその社員へ会話を 1 往復振る（AI 同士の交流。深さは 1 に限定）。

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/yukirawa/ai-office/server/internal/agents"
)

// RunInitiative は各エージェントに自発的な行動を 1 回させる。cron から定期実行される。
func (s *Server) RunInitiative(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	brief := s.officeBrief()
	agentsMap := s.agentsCopy()
	for _, id := range sortedAgentIDs(agentsMap) {
		ia, ok := agentsMap[id].(agents.InitiativeAgent)
		if !ok {
			continue
		}
		text := strings.TrimSpace(ia.Initiative(ctx, brief))
		if text == "" {
			continue
		}
		if err := s.Notify(ctx, channelDefault, id, text); err != nil {
			s.log.Warn("自発発言の投稿に失敗しました", "employee_id", id, "error", err)
			continue
		}
		s.log.Info("自発発言を投稿しました", "employee_id", id, "text", truncateRunes(text, 80))
		s.routeAgentMention(ctx, id, text)
	}
}

// routeAgentMention は自発発言が @id で始まる場合に、その社員へ会話を 1 往復振る。
// 相手の返答は通常の Converse（1 往復）で、そこからさらに連鎖しない（深さ 1）。
func (s *Server) routeAgentMention(ctx context.Context, fromID, text string) {
	target, body := parseMention(text)
	id := strings.ToLower(strings.TrimSpace(target))
	switch id {
	case "":
		return
	case "all":
		for tid, ag := range s.agentsCopy() {
			if tid != fromID {
				s.converseAsync(ag, channelDefault, body)
			}
		}
	default:
		if id == fromID {
			return
		}
		if ag := s.employee(id); ag != nil {
			s.converseAsync(ag, channelDefault, body)
		}
	}
}

// officeBrief は自発発言の材料になるオフィス状況を短くまとめる。
func (s *Server) officeBrief() string {
	var b strings.Builder
	fmt.Fprintf(&b, "在席: %s\n", strings.Join(nonNil(s.presence.Online()), ", "))

	if msgs, err := s.store.Messages(channelDefault, 8); err == nil && len(msgs) > 0 {
		b.WriteString("直近の発言（新しい順）:\n")
		for _, m := range msgs {
			if isPresenceNoise(m.FromID, m.Content) {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", m.FromID, truncateRunes(m.Content, 120))
		}
	}

	if tasks, err := s.store.Tasks("", 5); err == nil && len(tasks) > 0 {
		b.WriteString("最近のタスク:\n")
		for _, t := range tasks {
			fmt.Fprintf(&b, "- [%s] %s (担当 %s)\n", t.Status, truncateRunes(t.Title, 60), t.Assignee)
		}
	}
	return b.String()
}

// agentsCopy は登録済みエージェントのコピーを返す（ロック保持中の呼び出しを避けるため）。
func (s *Server) agentsCopy() map[string]agents.Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]agents.Agent, len(s.employees))
	for k, v := range s.employees {
		out[k] = v
	}
	return out
}

// sortedAgentIDs はマップのキーを昇順で返す（実行順を決定的にする）。
func sortedAgentIDs(m map[string]agents.Agent) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
