package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	client := NewAnthropic("k", srv.URL)
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
