package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// anthropicVersion は Anthropic Messages API が要求する必須ヘッダ値。
	anthropicVersion = "2023-06-01"
	// defaultMaxTokens はリクエストで MaxTokens が未指定（0 以下）の場合に使う値。
	defaultMaxTokens = 1024
	// defaultBaseURL は baseURL が空のときに使う既定エンドポイント。
	defaultBaseURL = "https://api.anthropic.com"
	// defaultHTTPTimeout は 1 リクエストの上限時間。
	defaultHTTPTimeout = 60 * time.Second
	// maxErrorBodyRunes はエラーレスポンス本文をログ/エラーに含める際の上限文字数。
	maxErrorBodyRunes = 512
)

// AnthropicClient は Anthropic Messages API の実装。
type AnthropicClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewAnthropic は AnthropicClient を生成する。
// baseURL が空の場合は https://api.anthropic.com を使う。
// baseURL はテストで httptest.NewServer を指せるように差し替え可能にしている。
func NewAnthropic(apiKey, baseURL string) *AnthropicClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &AnthropicClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// anthropicRequest は Messages API のリクエストボディ。
// Request / Message / Tool はすでに wire 形式の JSON タグを持つため、そのまま埋め込む。
type anthropicRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsage          `json:"usage"`
	StopReason string                  `json:"stop_reason"`
}

// Chat は Messages API に POST し、レスポンスを Response に変換する。
// 非 2xx はエラーとし、ステータスコードと本文（切り詰め）を含める。
func (c *AnthropicClient) Chat(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	body := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// ctx キャンセルもここに含まれる（context.Canceled / DeadlineExceeded をラップ）。
		return Response{}, fmt.Errorf("llm: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: read response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Response{}, fmt.Errorf("llm: anthropic api error: status %d: %s", resp.StatusCode, truncateRunes(respBody, maxErrorBodyRunes))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}

	// content 配列の text ブロックを連結する（tool_use 等の他ブロックは無視）。
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return Response{
		Text:         text.String(),
		Model:        parsed.Model,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
		StopReason:   parsed.StopReason,
	}, nil
}

// truncateRunes はエラー本文を最大 n 文字（ルーン単位）に切り詰める。
// マルチバイト文字を壊さないように rune で扱う。
func truncateRunes(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…(truncated)"
}
