package api

// chat.go は Phase 4.3 の chat 役への入力口。
//
// chat 役（agents.ChatAgent）は Ollama ではなくサーバーの llm.Client で応答する。
// オーナーは POST /api/chat で発言でき、雑談 cron（main）も同じ経路で投稿する。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yukirawa/ai-office/server/internal/agents"
)

// SetChat は chat 役エージェントを設定する（Phase 4.3）。
func (s *Server) SetChat(c *agents.ChatAgent) {
	s.mu.Lock()
	s.chat = c
	s.mu.Unlock()
}

// chatAgent は現在の chat 役を返す（未設定なら nil）。
func (s *Server) chatAgent() *agents.ChatAgent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chat
}

// handleChat はオーナーの発言を chat 役に渡す。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON の解釈に失敗しました")
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		writeJSONError(w, http.StatusBadRequest, "message は必須です")
		return
	}

	chat := s.chatAgent()
	if chat == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "chat 役が設定されていません")
		return
	}
	if !chat.Post(msg) {
		writeJSONError(w, http.StatusServiceUnavailable, "chat 役の受信箱が満杯です")
		return
	}

	s.log.Info("chat 役へメッセージを渡しました", "length", len([]rune(msg)))
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
}
