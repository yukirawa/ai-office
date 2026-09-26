package api

// llm.go は実 API キー / モデルを診断するためのエンドポイントを提供する。
//
// 設計書 §4.3 のエンドポイント一覧に対する拡張（§12.2 と同じ扱い）。
// フルタスクを回す前に「キーが通るか」「モデル名が正しいか」を 1 コマンドで
// 確認できるようにするのが目的なので、ここでは副作用を持たない（永続化しない）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
)

// pingMaxTokens は /api/llm/ping が要求する最大トークン数。
// 接続確認だけなので小さく固定する（cfg.LLMMaxTokens はタスク実行用）。
const pingMaxTokens = 32

// pingTimeout は /api/llm/ping が 1 回の Chat に許す上限。
// 実 API の遅延を吸収しつつ、キー検証がぶら下がらない程度に短く固定する。
const pingTimeout = 60 * time.Second

// defaultLLMPingPrompt は /api/llm/ping に body が無いときの既定メッセージ。
const defaultLLMPingPrompt = "接続確認です。『pong』とだけ返してください。"

// SetLLM は LLM クライアントを後から差し込む（SetGitHub と同じパターン）。
// nil を渡すと未設定に戻る。
func (s *Server) SetLLM(c llm.Client) {
	s.mu.Lock()
	s.llmClient = c
	s.mu.Unlock()
}

// llm は現在の LLM クライアントを返す（未設定なら nil）。
func (s *Server) llm() llm.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llmClient
}

// llmInfoResponse は GET /api/llm の応答。
type llmInfoResponse struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	MaxTokens  int    `json:"max_tokens"`
	Timeout    string `json:"timeout"`
	Configured bool   `json:"configured"`
	Mock       bool   `json:"mock"`
}

// handleLLMInfo は現在の LLM 設定を返す。
//
// configured は「実際に Chat を呼べる可能性が高いか」を表す:
//   - provider が "mock" なら API キー不要なので true。
//   - それ以外は llm.Client が差し込まれている（= API キーが解決できた）ときだけ true。
//
// 厳密な API キーの有効性はここでは判定せず、/api/llm/ping で確認する。
func (s *Server) handleLLMInfo(w http.ResponseWriter, _ *http.Request) {
	provider := s.cfg.LLMProvider
	mock := provider == "mock"
	writeJSON(w, http.StatusOK, llmInfoResponse{
		Provider:   provider,
		Model:      s.cfg.LLMModel,
		MaxTokens:  s.cfg.LLMMaxTokens,
		Timeout:    s.cfg.LLMTimeout.String(),
		Configured: mock || s.llm() != nil,
		Mock:       mock,
	})
}

// handleLLMPing は 1 回だけ Chat を呼び、キー・モデルが使えるかを確認する。
//
// body は任意（{"message":"..."}）。未指定なら既定の短いプロンプトを使う。
func (s *Server) handleLLMPing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	// body は任意なので、空ボディ（EOF）はエラーにしない。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "JSON の解釈に失敗しました")
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		msg = defaultLLMPingPrompt
	}

	client := s.llm()
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "LLM クライアントが設定されていません",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()

	resp, err := client.Chat(ctx, llm.Request{
		Model:     s.cfg.LLMModel,
		Messages:  []llm.Message{{Role: "user", Content: msg}},
		MaxTokens: pingMaxTokens,
	})
	if err != nil {
		s.log.Error("LLM ping に失敗しました", "model", s.cfg.LLMModel, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	// 実 API が Model を空で返すことは稀だが、その場合は設定値で補う。
	model := resp.Model
	if model == "" {
		model = s.cfg.LLMModel
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"model":         model,
		"text":          resp.Text,
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
	})
}
