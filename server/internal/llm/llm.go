// Package llm は LLM API への最小限の HTTP ラッパーを提供する。
//
// 設計書 §6.2 に対応する。プロバイダごとの実装（Anthropic / OpenAI / Ollama）を
// 差し替え可能にするため Client インターフェースを定義し、テストでは
// MockClient を用いて実際のネットワークアクセスを行わない（§10 テスト方針）。
//
// 実装: Anthropic Messages API（anthropic.go）と、OpenAI 互換 API を提供する
// DeepSeek（deepseek.go）。共通のオプションとリトライ処理は options.go / retry.go に置く。
package llm

import (
	"context"
	"encoding/json"
)

// Message は会話の 1 メッセージ。Role は "user" または "assistant"。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Tool は LLM に渡すツール定義。
// Anthropic の wire 形式（input_schema）に合わせた JSON タグを付けている。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"input_schema,omitempty"`
}

// Request は 1 回の Chat 呼び出しに必要な入力。
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Response は Chat 呼び出しの結果。
type Response struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	StopReason   string
}

// Client は LLM プロバイダの抽象。テスト時にモックへ差し替えるための境界。
//
// 実装は ctx のキャンセルを尊重し、ネットワークエラーを戻り値の error として返す。
type Client interface {
	Chat(ctx context.Context, req Request) (Response, error)
}
