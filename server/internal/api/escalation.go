package api

// escalation.go は §16 のエスカレーションを実装する。
//
// 担当者（dev）が疑問を持つと、mgr 経由でオーナーへ質問が上がる:
//   dev --Escalate--> mgr --AskOwner--> api（保存 + question 配信）
//   オーナー --answer--> api --AnswerQuestion--> mgr --Escalate の戻り値--> dev
//
// 「管理職を通す」ため、質問は mgr 名義で保存・配信する。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// AskOwner は agents.OwnerChannel の実装。担当者の疑問をオーナーへ送る。
// 履歴用に #会議室 へ mgr 名義で保存し、接続中のクライアントへ question を配信する。
func (s *Server) AskOwner(_ context.Context, q agents.Question) error {
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return nil
	}
	// オーナー（TUI など）が接続していなければ待たずに即フォールバックさせる。
	// 長時間ブロックすると、タスクが無前提に滞留してしまうため。
	if !s.hub.hasClients() {
		return fmt.Errorf("api: オーナーが接続していないため質問できません")
	}
	ts := time.Now().UTC()
	content := fmt.Sprintf("【質問】%s より: %s", strings.TrimSpace(q.FromID), text)
	if _, err := s.store.InsertMessage(store.Message{
		Channel: channelDefault,
		FromID:  config.EmployeeManagerID,
		Content: content,
		TS:      ts,
	}); err != nil {
		return fmt.Errorf("api: 質問の保存に失敗しました: %w", err)
	}

	s.hub.broadcast(questionMsg{
		Type:   "question",
		ID:     q.ID,
		From:   q.FromID,
		Text:   text,
		TaskID: q.TaskID,
		TS:     formatTS(ts),
	})
	s.log.Info("オーナーへ質問を送りました", "question_id", q.ID, "from", q.FromID, "task_id", q.TaskID)
	return nil
}

// AnswerQuestion はオーナーの回答を mgr に渡し、待機中の担当者へ届ける。
func (s *Server) AnswerQuestion(questionID, text string) {
	m := s.Manager()
	if m == nil {
		return
	}
	if !m.Answer(questionID, text) {
		s.log.Debug("待機の無い回答を無視しました", "question_id", questionID)
	}
}
