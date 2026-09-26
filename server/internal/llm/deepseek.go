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

// defaultDeepSeekBaseURL は baseURL が空のときに使う既定エンドポイント。
// DeepSeek は OpenAI 互換 API を提供する。
const defaultDeepSeekBaseURL = "https://api.deepseek.com"

// DeepSeekClient は DeepSeek（OpenAI 互換）API の実装。
//
// リトライ方針は AnthropicClient と同じ。HTTP 429/500/502/503/504 と
// トランスポート（ネットワーク）エラーのみ再試行し、その他の 4xx や
// ctx のキャンセル/期限切れは再試行しない。
type DeepSeekClient struct {
	apiKey         string
	baseURL        string
	httpClient     *http.Client
	maxRetries     int
	retryBaseDelay time.Duration
}

// DeepSeekClient が Client を満たすことをコンパイル時に保証する。
var _ Client = (*DeepSeekClient)(nil)

// NewDeepSeek は DeepSeekClient を生成する。
// baseURL が空の場合は https://api.deepseek.com を使う。
// baseURL はテストで httptest.NewServer を指せるように差し替え可能にしている。
//
// オプション未指定時の既定値: timeout 120s、maxRetries 3、retryBaseDelay 500ms。
func NewDeepSeek(apiKey, baseURL string, opts ...Option) *DeepSeekClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultDeepSeekBaseURL
	}
	o := newClientOptions(opts...)
	return &DeepSeekClient{
		apiKey:         apiKey,
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: o.timeout},
		maxRetries:     o.maxRetries,
		retryBaseDelay: o.retryBaseDelay,
	}
}

// String は DeepSeekClient の人間可読な表現を返す。
// API キーなどのシークレットは絶対に含めない（ログ出力を想定）。
func (c *DeepSeekClient) String() string {
	return fmt.Sprintf("deepseek(baseURL=%s, timeout=%s, maxRetries=%d)",
		c.baseURL, c.httpClient.Timeout, c.maxRetries)
}

// deepseekToolFunction / deepseekTool は OpenAI 互換の tools 形式。
// llm.Tool の Schema を parameters にそのまま埋め込む。
type deepseekToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type deepseekTool struct {
	Type     string               `json:"type"`
	Function deepseekToolFunction `json:"function"`
}

// deepseekRequest は /chat/completions のリクエストボディ。
// stream は常に false（同期応答のみ扱う）ため omitempty を付けない。
type deepseekRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Stream    bool           `json:"stream"`
	Messages  []Message      `json:"messages"`
	Tools     []deepseekTool `json:"tools,omitempty"`
}

type deepseekUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type deepseekChoice struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type deepseekResponse struct {
	Model   string           `json:"model"`
	Choices []deepseekChoice `json:"choices"`
	Usage   deepseekUsage    `json:"usage"`
}

// toDeepSeekTools は llm.Tool を OpenAI 互換の tools 形式へ変換する。
// tools が空の場合は nil を返し、JSON では tools フィールドごと省略される。
func toDeepSeekTools(tools []Tool) []deepseekTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]deepseekTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, deepseekTool{
			Type: "function",
			Function: deepseekToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	return out
}

// Chat は /chat/completions に POST し、レスポンスを Response に変換する。
//
// System は OpenAI 互換の system ロールとして messages の先頭に差し込む。
// 429/500/502/503/504 とトランスポートエラーは指数バックオフで再試行する。
// Retry-After ヘッダ（秒数 or HTTP-date）があればそれを優先し、最大 30s に丸める。
// リクエストボディは 1 度だけ marshal し、試行ごとに HTTP リクエストを組み立て直す。
// 非 2xx はエラーとし、ステータスコード・本文（切り詰め）・対処ヒントを含める。
func (c *DeepSeekClient) Chat(ctx context.Context, req Request) (Response, error) {
	if c.apiKey == "" {
		return Response{}, errors.New("llm: deepseek: API キーが未設定です (API キーを確認してください)")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	messages := make([]Message, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, Message{Role: "system", Content: req.System})
	}
	messages = append(messages, req.Messages...)

	body := deepseekRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		Stream:    false,
		Messages:  messages,
		Tools:     toDeepSeekTools(req.Tools),
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
		if !res.retryable || attempt == attempts-1 {
			return Response{}, res.err
		}
		retryAfter, hasAfter = res.retryAfter, res.hasAfter
	}
	return Response{}, errors.New("llm: retry loop exhausted")
}

// doAttempt は 1 回の HTTP 試行を行い、リトライ可否を含む結果を返す。
func (c *DeepSeekClient) doAttempt(ctx context.Context, payload []byte, requestedModel string) httpResult {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return httpResult{err: fmt.Errorf("llm: build request: %w", err)}
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+c.apiKey)

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
			err:        apiStatusError("deepseek", resp.StatusCode, respBody),
		}
	}

	var parsed deepseekResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return httpResult{err: fmt.Errorf("llm: decode response: %w", err)}
	}

	var text, stopReason string
	if len(parsed.Choices) > 0 {
		text = parsed.Choices[0].Message.Content
		stopReason = parsed.Choices[0].FinishReason
	}

	// API が model を省略した場合はリクエスト時のモデル名で補う。
	model := parsed.Model
	if model == "" {
		model = requestedModel
	}

	return httpResult{resp: Response{
		Text:         text,
		Model:        model,
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
		StopReason:   stopReason,
	}}
}

// Models は API キーで利用可能なモデル ID の一覧を返す（GET /models）。
// 実キー投入時に「どのモデル名を OFFICE_LLM_MODEL に設定すべきか」を確認する用途。
// トークンを消費しない。
func (c *DeepSeekClient) Models(ctx context.Context) ([]string, error) {
	if c.apiKey == "" {
		return nil, errors.New("llm: deepseek: API キーが未設定です (API キーを確認してください)")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("authorization", "Bearer "+c.apiKey)

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
		return nil, apiStatusError("deepseek", resp.StatusCode, body)
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
