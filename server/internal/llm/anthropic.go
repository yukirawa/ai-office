package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// anthropicVersion は Anthropic Messages API が要求する必須ヘッダ値。
	anthropicVersion = "2023-06-01"
	// defaultBaseURL は baseURL が空のときに使う既定エンドポイント。
	defaultBaseURL = "https://api.anthropic.com"
)

// AnthropicClient は Anthropic Messages API の実装。
//
// リトライ方針: HTTP 429/500/502/503/504 とトランスポート（ネットワーク）エラーのみ
// 再試行する。その他の 4xx や ctx のキャンセル/期限切れは再試行しない。
type AnthropicClient struct {
	apiKey         string
	baseURL        string
	httpClient     *http.Client
	maxRetries     int
	retryBaseDelay time.Duration
}

// NewAnthropic は AnthropicClient を生成する。
// baseURL が空の場合は https://api.anthropic.com を使う。
// baseURL はテストで httptest.NewServer を指せるように差し替え可能にしている。
//
// オプション未指定時の既定値: timeout 120s、maxRetries 3、retryBaseDelay 500ms。
// 後方互換のため opts は可変長にしている。
func NewAnthropic(apiKey, baseURL string, opts ...Option) *AnthropicClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	o := newClientOptions(opts...)
	return &AnthropicClient{
		apiKey:         apiKey,
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: o.timeout},
		maxRetries:     o.maxRetries,
		retryBaseDelay: o.retryBaseDelay,
	}
}

// String は AnthropicClient の人間可読な表現を返す。
// API キーなどのシークレットは絶対に含めない（ログ出力を想定）。
func (c *AnthropicClient) String() string {
	return fmt.Sprintf("anthropic(baseURL=%s, timeout=%s, maxRetries=%d)",
		c.baseURL, c.httpClient.Timeout, c.maxRetries)
}

// Models は API キーで利用可能なモデル ID の一覧を返す（GET /v1/models）。
// 実キー投入時に「どのモデル名を OFFICE_LLM_MODEL に設定すべきか」を確認する用途。
// トークンを消費しない。
func (c *AnthropicClient) Models(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, apiStatusError("anthropic", resp.StatusCode, body)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
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
//
// 429/500/502/503/504 とトランスポートエラーは指数バックオフで再試行する。
// Retry-After ヘッダ（秒数 or HTTP-date）があればそれを優先し、最大 30s に丸める。
// リクエストボディは 1 度だけ marshal し、試行ごとに HTTP リクエストを組み立て直す。
// 非 2xx はエラーとし、ステータスコード・本文（切り詰め）・対処ヒントを含める。
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

	attempts := 1 + c.maxRetries
	var (
		retryAfter time.Duration
		hasAfter   bool
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := computeRetryDelay(c.retryBaseDelay, attempt, retryAfter, hasAfter)
			if err := waitContext(ctx, delay); err != nil {
				return Response{}, fmt.Errorf("llm: request failed: %w", err)
			}
		}

		res := c.doAttempt(ctx, payload, req.Model)
		if res.err == nil {
			return res.resp, nil
		}
		// リトライ不可、または最後の試行なら最終エラーを返す。
		// ログ出力は呼び出し側の責務（このパッケージは logger を持たない）。
		if !res.retryable || attempt == attempts-1 {
			return Response{}, res.err
		}
		retryAfter, hasAfter = res.retryAfter, res.hasAfter
	}
	return Response{}, errors.New("llm: retry loop exhausted")
}

// doAttempt は 1 回の HTTP 試行を行い、リトライ可否を含む結果を返す。
func (c *AnthropicClient) doAttempt(ctx context.Context, payload []byte, requestedModel string) httpResult {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return httpResult{err: fmt.Errorf("llm: build request: %w", err)}
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// ctx のキャンセル/期限切れはリトライせず、その他のトランスポートエラーは再試行する。
		return transportError(ctx, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpResult{err: fmt.Errorf("llm: read response: %w", err)}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		retryAfter, hasAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return httpResult{
			retryable:  retryableStatus(resp.StatusCode),
			retryAfter: retryAfter,
			hasAfter:   hasAfter,
			err:        apiStatusError("anthropic", resp.StatusCode, respBody),
		}
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return httpResult{err: fmt.Errorf("llm: decode response: %w", err)}
	}

	// content 配列の text ブロックを連結する（tool_use 等の他ブロックは無視）。
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	// API が model を省略した場合はリクエスト時のモデル名で補う。
	model := parsed.Model
	if model == "" {
		model = requestedModel
	}

	return httpResult{resp: Response{
		Text:         text.String(),
		Model:        model,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
		StopReason:   parsed.StopReason,
	}}
}
