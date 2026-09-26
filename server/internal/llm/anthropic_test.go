package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer はリクエストを記録し、固定レスポンスを返す httptest サーバを立てる。
func newTestServer(t *testing.T, status int, body string) (*httptest.Server, *recordedHTTP) {
	t.Helper()
	rec := &recordedHTTP{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.headers = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("リクエスト読み取りに失敗: %v", err)
		}
		rec.body = raw
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type recordedHTTP struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

func TestAnthropicChatRequestShape(t *testing.T) {
	srv, rec := newTestServer(t, http.StatusOK, `{
		"model": "claude-3-5-haiku-latest",
		"content": [{"type": "text", "text": "計画です"}],
		"usage": {"input_tokens": 12, "output_tokens": 34},
		"stop_reason": "end_turn"
	}`)

	client := NewAnthropic("secret-key", srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	req := Request{
		Model:     "claude-3-5-haiku-latest",
		System:    "あなたは管理職です。",
		Messages:  []Message{{Role: "user", Content: "計画を立てて"}},
		Tools:     []Tool{{Name: "search", Description: "検索する", Schema: schema}},
		MaxTokens: 256,
	}

	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}

	if rec.method != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.method)
	}
	if rec.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", rec.path)
	}
	if got := rec.headers.Get("x-api-key"); got != "secret-key" {
		t.Errorf("x-api-key = %q, want secret-key", got)
	}
	if got := rec.headers.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
	}
	if got := rec.headers.Get("content-type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content-type = %q, want application/json", got)
	}

	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("送信ボディの JSON パースに失敗: %v (body=%s)", err, rec.body)
	}
	if got := sent["model"]; got != "claude-3-5-haiku-latest" {
		t.Errorf("model = %v", got)
	}
	if got := sent["max_tokens"]; got != float64(256) {
		t.Errorf("max_tokens = %v, want 256", got)
	}
	if got := sent["system"]; got != "あなたは管理職です。" {
		t.Errorf("system = %v", got)
	}
	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", sent["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "計画を立てて" {
		t.Errorf("messages[0] = %v", first)
	}
	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", sent["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "search" {
		t.Errorf("tools[0].name = %v", tool["name"])
	}
	if tool["input_schema"] == nil {
		t.Errorf("tools[0].input_schema が送信されていません: %v", tool)
	}
}

func TestAnthropicChatOmitsEmptySystemAndTools(t *testing.T) {
	srv, rec := newTestServer(t, http.StatusOK, `{
		"model": "m",
		"content": [{"type": "text", "text": "ok"}],
		"usage": {"input_tokens": 1, "output_tokens": 1},
		"stop_reason": "end_turn"
	}`)

	client := NewAnthropic("k", srv.URL)
	if _, err := client.Chat(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("JSON パースに失敗: %v", err)
	}
	if _, exists := sent["system"]; exists {
		t.Errorf("空の system は省略されるべきです: %v", sent["system"])
	}
	if _, exists := sent["tools"]; exists {
		t.Errorf("空の tools は省略されるべきです: %v", sent["tools"])
	}
	// MaxTokens 未指定時は既定値が入る。
	if got := sent["max_tokens"]; got != float64(defaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got, defaultMaxTokens)
	}
}

func TestAnthropicChatResponseParsing(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusOK, `{
		"model": "claude-3-5-sonnet",
		"content": [
			{"type": "text", "text": "前半。"},
			{"type": "thinking", "text": "無視されるべき"},
			{"type": "text", "text": "後半。"}
		],
		"usage": {"input_tokens": 100, "output_tokens": 50},
		"stop_reason": "max_tokens"
	}`)

	client := NewAnthropic("k", srv.URL)
	resp, err := client.Chat(context.Background(), Request{Model: "claude-3-5-sonnet"})
	if err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}

	if resp.Text != "前半。後半。" {
		t.Errorf("Text = %q, want %q", resp.Text, "前半。後半。")
	}
	if resp.Model != "claude-3-5-sonnet" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 50 {
		t.Errorf("tokens = (%d,%d), want (100,50)", resp.InputTokens, resp.OutputTokens)
	}
	if resp.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
}

func TestAnthropicChatErrorStatus(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`)

	client := NewAnthropic("bad", srv.URL)
	_, err := client.Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("非 2xx ではエラーを返すべきです")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("エラーにステータスコードが含まれていません: %v", msg)
	}
	if !strings.Contains(msg, "invalid x-api-key") {
		t.Errorf("エラーに本文が含まれていません: %v", msg)
	}
}

func TestAnthropicChatErrorBodyTruncated(t *testing.T) {
	long := strings.Repeat("あ", maxErrorBodyRunes+100)
	srv, _ := newTestServer(t, http.StatusInternalServerError, long)

	// 500 はリトライ対象なので、切り詰め検証を高速化するため無効化する。
	client := NewAnthropic("k", srv.URL, WithMaxRetries(0))
	_, err := client.Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("エラーを返すべきです")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("長い本文は切り詰められるべきです: %v", err)
	}
	// rune 数が上限付近に収まっていること（バイト数ではなく文字数）。
	if runes := []rune(err.Error()); len(runes) > maxErrorBodyRunes+100 {
		t.Errorf("エラー本文が長すぎます: %d runes", len(runes))
	}
}

func TestAnthropicChatContextCanceled(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusOK, `{
		"model": "m",
		"content": [{"type": "text", "text": "ok"}],
		"usage": {"input_tokens": 1, "output_tokens": 1},
		"stop_reason": "end_turn"
	}`)

	client := NewAnthropic("k", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	_, err := client.Chat(ctx, Request{Model: "m"})
	if err == nil {
		t.Fatal("キャンセル済み ctx ではエラーを返すべきです")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("context のエラーをラップすべきです: %v", err)
	}
}

// okBody は 200 応答の最小 JSON。
const okBody = `{
	"model": "m",
	"content": [{"type": "text", "text": "ok"}],
	"usage": {"input_tokens": 1, "output_tokens": 1},
	"stop_reason": "end_turn"
}`

func TestAnthropicDefaults(t *testing.T) {
	client := NewAnthropic("k", "")
	if client.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, defaultBaseURL)
	}
	if client.httpClient.Timeout != defaultHTTPTimeout {
		t.Errorf("timeout = %v, want %v", client.httpClient.Timeout, defaultHTTPTimeout)
	}
	if client.maxRetries != defaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", client.maxRetries, defaultMaxRetries)
	}
	if client.retryBaseDelay != defaultRetryBaseDelay {
		t.Errorf("retryBaseDelay = %v, want %v", client.retryBaseDelay, defaultRetryBaseDelay)
	}
}

func TestAnthropicRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)

	client := NewAnthropic("k", srv.URL, WithRetryBaseDelay(time.Millisecond))
	resp, err := client.Chat(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("429 後の成功を期待しましたがエラー: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("サーバが受けたリクエスト数 = %d, want 2", got)
	}
}

func TestAnthropicExhaustsRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)

	// 初回 + maxRetries(2) = 3 回で打ち切る。
	client := NewAnthropic("k", srv.URL, WithMaxRetries(2), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("リトライを使い切ったらエラーを返すべきです")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("エラーにステータスコードが含まれていません: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("サーバが受けたリクエスト数 = %d, want 3", got)
	}
}

func TestAnthropicDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"no"}}`))
			}))
			t.Cleanup(srv.Close)

			client := NewAnthropic("k", srv.URL, WithRetryBaseDelay(time.Millisecond))
			_, err := client.Chat(context.Background(), Request{Model: "m"})
			if err == nil {
				t.Fatalf("status %d ではエラーを返すべきです", status)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("status %d: リクエスト数 = %d, want 1（リトライ禁止）", status, got)
			}
		})
	}
}

func TestAnthropicHonorsRetryAfter(t *testing.T) {
	// 1 回目は 503 + Retry-After: 1（秒）。
	// バックオフ基準は 1ms なので、Retry-After を無視していれば 1ms 後に
	// リトライして 2 回目で成功してしまう。ctx を 200ms で切ることで、
	// 「実際に 200ms 以上待機している」= Retry-After を尊重していることを検証する。
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"overloaded_error","message":"busy"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)

	client := NewAnthropic("k", srv.URL, WithRetryBaseDelay(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Chat(ctx, Request{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry-After 待機中に ctx が期限切れになるはずです: err=%v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("サーバが受けたリクエスト数 = %d, want 1（1 秒待ってからリトライするはず）", got)
	}
}

func TestAnthropicRetryAfterOverridesBackoff(t *testing.T) {
	// Retry-After: 0 は「即時リトライ」。バックオフ基準を 1 分にしても
	// 即座に再試行・成功することを確認する（Retry-After 優先の検証）。
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)

	client := NewAnthropic("k", srv.URL, WithRetryBaseDelay(time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := client.Chat(ctx, Request{Model: "m"}); err != nil {
		t.Fatalf("Retry-After: 0 は即時リトライするはずです: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("サーバが受けたリクエスト数 = %d, want 2", got)
	}
}

func TestAnthropicTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)

	client := NewAnthropic("k", srv.URL,
		WithTimeout(50*time.Millisecond),
		WithMaxRetries(0),
		WithRetryBaseDelay(time.Millisecond),
	)
	if _, err := client.Chat(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("タイムアウトではエラーを返すべきです")
	}
}

func TestAnthropicStringOmitsAPIKey(t *testing.T) {
	const key = "sk-ant-super-secret-key"
	client := NewAnthropic(key, "https://example.test", WithTimeout(3*time.Second), WithMaxRetries(5))
	s := client.String()
	if strings.Contains(s, key) {
		t.Fatalf("String() に API キーが含まれています: %q", s)
	}
	for _, want := range []string{"anthropic(", "baseURL=https://example.test", "timeout=3s", "maxRetries=5"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() に %q が含まれていません: %q", want, s)
		}
	}
}

func TestAnthropicModelFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		// model フィールドを省略する。
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "ok"}],
			"usage": {"input_tokens": 1, "output_tokens": 1},
			"stop_reason": "end_turn"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := NewAnthropic("k", srv.URL, WithMaxRetries(0))
	resp, err := client.Chat(context.Background(), Request{Model: "claude-requested"})
	if err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}
	if resp.Model != "claude-requested" {
		t.Errorf("Model = %q, want リクエスト時の claude-requested", resp.Model)
	}
}

func TestAnthropicErrorHints(t *testing.T) {
	cases := []struct {
		status int
		hint   string
	}{
		{http.StatusUnauthorized, "API キーを確認してください"},
		{http.StatusNotFound, "モデル名を確認してください"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv, _ := newTestServer(t, tc.status, `{"error":{"message":"x"}}`)
			client := NewAnthropic("k", srv.URL, WithMaxRetries(0))
			_, err := client.Chat(context.Background(), Request{Model: "m"})
			if err == nil {
				t.Fatalf("status %d ではエラーを返すべきです", tc.status)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("エラーに対処ヒント %q が含まれていません: %v", tc.hint, err)
			}
		})
	}
}
