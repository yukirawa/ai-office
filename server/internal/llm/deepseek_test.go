package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dsRecorded は httptest サーバが受け取った 1 リクエストの記録。
type dsRecorded struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

// newDSRecorder は固定レスポンスを返しつつリクエストを記録する httptest サーバを立てる。
func newDSRecorder(t *testing.T, status int, body string) (*httptest.Server, *dsRecorded) {
	t.Helper()
	rec := &dsRecorded{}
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

// dsOKBody は 200 応答の最小 JSON（OpenAI 互換）。
const dsOKBody = `{
	"model": "deepseek-chat",
	"choices": [{"message": {"role": "assistant", "content": "了解"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 7, "completion_tokens": 11}
}`

func TestDeepSeekChatRequestShape(t *testing.T) {
	srv, rec := newDSRecorder(t, http.StatusOK, dsOKBody)

	client := NewDeepSeek("secret-key", srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	req := Request{
		Model:     "deepseek-chat",
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
	if rec.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", rec.path)
	}
	if got := rec.headers.Get("authorization"); got != "Bearer secret-key" {
		t.Errorf("authorization = %q, want Bearer secret-key", got)
	}
	if got := rec.headers.Get("content-type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content-type = %q, want application/json", got)
	}

	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("送信ボディの JSON パースに失敗: %v (body=%s)", err, rec.body)
	}
	if got := sent["model"]; got != "deepseek-chat" {
		t.Errorf("model = %v", got)
	}
	if got := sent["max_tokens"]; got != float64(256) {
		t.Errorf("max_tokens = %v, want 256", got)
	}
	if stream, ok := sent["stream"].(bool); !ok || stream {
		t.Errorf("stream = %v, want false", sent["stream"])
	}

	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v", sent["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "あなたは管理職です。" {
		t.Errorf("messages[0] = %v, want system ロール", first)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "計画を立てて" {
		t.Errorf("messages[1] = %v", second)
	}

	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", sent["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tools[0].type = %v, want function", tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function = %v", tool["function"])
	}
	if fn["name"] != "search" || fn["description"] != "検索する" {
		t.Errorf("tools[0].function = %v", fn)
	}
	if fn["parameters"] == nil {
		t.Errorf("tools[0].function.parameters が送信されていません: %v", fn)
	}
}

func TestDeepSeekChatOmitsSystemAndTools(t *testing.T) {
	srv, rec := newDSRecorder(t, http.StatusOK, dsOKBody)

	client := NewDeepSeek("k", srv.URL)
	if _, err := client.Chat(context.Background(), Request{
		Model:    "deepseek-chat",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("JSON パースに失敗: %v", err)
	}
	if _, exists := sent["tools"]; exists {
		t.Errorf("空の tools は省略されるべきです: %v", sent["tools"])
	}
	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("System 無しでは messages はそのままのはずです: %v", sent["messages"])
	}
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages[0] = %v", msgs[0])
	}
	// MaxTokens 未指定時は既定値が入る。
	if got := sent["max_tokens"]; got != float64(defaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got, defaultMaxTokens)
	}
}

func TestDeepSeekChatResponseParsing(t *testing.T) {
	srv, _ := newDSRecorder(t, http.StatusOK, `{
		"model": "deepseek-reasoner",
		"choices": [{"message": {"role": "assistant", "content": "回答本文"}, "finish_reason": "length"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 50}
	}`)

	client := NewDeepSeek("k", srv.URL)
	resp, err := client.Chat(context.Background(), Request{Model: "deepseek-reasoner"})
	if err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}

	if resp.Text != "回答本文" {
		t.Errorf("Text = %q, want 回答本文", resp.Text)
	}
	if resp.Model != "deepseek-reasoner" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 50 {
		t.Errorf("tokens = (%d,%d), want (100,50)", resp.InputTokens, resp.OutputTokens)
	}
	if resp.StopReason != "length" {
		t.Errorf("StopReason = %q, want length", resp.StopReason)
	}
}

func TestDeepSeekModelFallback(t *testing.T) {
	srv, _ := newDSRecorder(t, http.StatusOK, `{
		"choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}],
		"usage": {}
	}`)

	client := NewDeepSeek("k", srv.URL, WithMaxRetries(0))
	resp, err := client.Chat(context.Background(), Request{Model: "deepseek-requested"})
	if err != nil {
		t.Fatalf("Chat がエラーを返しました: %v", err)
	}
	if resp.Model != "deepseek-requested" {
		t.Errorf("Model = %q, want リクエスト時の deepseek-requested", resp.Model)
	}
}

func TestDeepSeekDefaults(t *testing.T) {
	client := NewDeepSeek("k", "")
	if client.baseURL != defaultDeepSeekBaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, defaultDeepSeekBaseURL)
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

func TestDeepSeekRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(dsOKBody))
	}))
	t.Cleanup(srv.Close)

	client := NewDeepSeek("k", srv.URL, WithRetryBaseDelay(time.Millisecond))
	resp, err := client.Chat(context.Background(), Request{Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("429 後の成功を期待しましたがエラー: %v", err)
	}
	if resp.Text != "了解" {
		t.Errorf("Text = %q, want 了解", resp.Text)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("サーバが受けたリクエスト数 = %d, want 2", got)
	}
}

func TestDeepSeekExhaustsRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(srv.Close)

	// 初回 + maxRetries(2) = 3 回で打ち切る。
	client := NewDeepSeek("k", srv.URL, WithMaxRetries(2), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Chat(context.Background(), Request{Model: "deepseek-chat"})
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

func TestDeepSeekDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewDeepSeek("k", srv.URL, WithRetryBaseDelay(time.Millisecond))
	_, err := client.Chat(context.Background(), Request{Model: "deepseek-chat"})
	if err == nil {
		t.Fatal("400 ではエラーを返すべきです")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("リクエスト数 = %d, want 1（リトライ禁止）", got)
	}
}

func TestDeepSeekMissingAPIKey(t *testing.T) {
	// API キー未設定はネットワークに出る前にエラーにする（ヒント付き）。
	client := NewDeepSeek("", "https://example.invalid")
	_, err := client.Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("API キー未設定ではエラーを返すべきです")
	}
	if !strings.Contains(err.Error(), "API キーを確認してください") {
		t.Errorf("ヒントが含まれていません: %v", err)
	}

	if _, err := client.Models(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "API キーを確認してください") {
		t.Errorf("Models も API キーのヒントを返すべきです: %v", err)
	}
}

func TestDeepSeekErrorHints(t *testing.T) {
	cases := []struct {
		status int
		hint   string
	}{
		{http.StatusUnauthorized, "API キーを確認してください"},
		{http.StatusNotFound, "モデル名を確認してください"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv, _ := newDSRecorder(t, tc.status, `{"error":{"message":"x"}}`)
			client := NewDeepSeek("k", srv.URL, WithMaxRetries(0))
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

func TestDeepSeekModels(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"},{"id":""}]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewDeepSeek("secret-key", srv.URL)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("authorization = %q, want Bearer secret-key", gotAuth)
	}
	if len(models) != 2 || models[0] != "deepseek-chat" || models[1] != "deepseek-reasoner" {
		t.Errorf("models = %v, want [deepseek-chat deepseek-reasoner]", models)
	}
}

func TestDeepSeekStringOmitsAPIKey(t *testing.T) {
	const key = "sk-deepseek-super-secret-key"
	client := NewDeepSeek(key, "https://example.test", WithTimeout(3*time.Second), WithMaxRetries(5))
	s := client.String()
	if strings.Contains(s, key) {
		t.Fatalf("String() に API キーが含まれています: %q", s)
	}
	for _, want := range []string{"deepseek(", "baseURL=https://example.test", "timeout=3s", "maxRetries=5"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() に %q が含まれていません: %q", want, s)
		}
	}
}

func TestDeepSeekContextCanceled(t *testing.T) {
	srv, _ := newDSRecorder(t, http.StatusOK, dsOKBody)

	client := NewDeepSeek("k", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	_, err := client.Chat(ctx, Request{Model: "deepseek-chat"})
	if err == nil {
		t.Fatal("キャンセル済み ctx ではエラーを返すべきです")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("context のエラーをラップすべきです: %v", err)
	}
}
