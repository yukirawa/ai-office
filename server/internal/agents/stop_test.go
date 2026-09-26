package agents

// stop_test.go は終端判定（isTerminalStop）のプロバイダ差異と、
// それに基づく 1 ターン完了 / 継続の挙動を検証する。
//
// 背景: 以前は Anthropic の停止理由（end_turn 等）しか終端と見なしておらず、
// DeepSeek / OpenAI の "stop" を「継続」と誤判定して max_turns まで回り続ける
// 不具合があった（実機ログで guard=max_turns が発生）。その回帰防止。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

func TestIsTerminalStop(t *testing.T) {
	cases := map[string]bool{
		"":               true, // 未設定は終端扱い
		"stop":           true, // OpenAI / DeepSeek
		"end":            true,
		"end_turn":       true, // Anthropic
		"stop_sequence":  true,
		"content_filter": true,
		"unknown_reason": true, // 不明は安全側で終端
		"length":         false,
		"max_tokens":     false, // Anthropic
		"tool_use":       false,
		"tool_calls":     false,
		"pause_turn":     false,
	}
	for reason, want := range cases {
		if got := isTerminalStop(reason); got != want {
			t.Errorf("isTerminalStop(%q) = %v, want %v", reason, got, want)
		}
	}
}

// scriptedClient は呼び出しごとに指定した StopReason を返すテスト用クライアント。
type scriptedClient struct {
	mu      sync.Mutex
	calls   int
	reasons []string
}

func (s *scriptedClient) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason := ""
	if s.calls < len(s.reasons) {
		reason = s.reasons[s.calls]
	}
	s.calls++
	return llm.Response{Text: "応答テキスト", Model: "scripted", StopReason: reason}, nil
}

func (s *scriptedClient) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// recordNotifier は通知を記録するだけの Notifier。
type recordNotifier struct {
	mu      sync.Mutex
	notices []string
}

func (r *recordNotifier) Notify(_ context.Context, _, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, text)
	return nil
}

func (r *recordNotifier) SetAgentState(string, string) {}

func TestChatStopsOnOpenAIStopReason(t *testing.T) {
	client := &scriptedClient{reasons: []string{"stop"}}
	chat := NewChatAgent("chat", persona.Load(`{"name":"チャット"}`), client, &recordNotifier{}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)

	if !chat.Post("こんにちは") {
		t.Fatal("Post に失敗しました")
	}
	waitCalls(t, client, 1)

	// "stop" は終端なので追加の呼び出しは起きない。
	time.Sleep(50 * time.Millisecond)
	if got := client.Calls(); got != 1 {
		t.Errorf("chat の LLM 呼び出し回数 = %d, want 1（stop を継続と誤判定している）", got)
	}
}

func TestChatContinuesOnLength(t *testing.T) {
	client := &scriptedClient{reasons: []string{"length", "stop"}}
	chat := NewChatAgent("chat", persona.Load(`{"name":"チャット"}`), client, &recordNotifier{}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)

	if !chat.Post("続けて") {
		t.Fatal("Post に失敗しました")
	}
	// "length" は継続、次の "stop" で終端 → 2 回呼ばれる。
	waitCalls(t, client, 2)
}

func TestManagerStopsOnOpenAIStopReason(t *testing.T) {
	client := &scriptedClient{reasons: []string{"stop"}}
	mgr := NewManager("mgr", persona.Load(`{"name":"ミカ"}`), client, &recordNotifier{}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	if !mgr.Post(Task{ID: "t1", Title: "テスト", From: "owner"}) {
		t.Fatal("Post に失敗しました")
	}
	waitCalls(t, client, 1)

	time.Sleep(50 * time.Millisecond)
	if got := client.Calls(); got != 1 {
		t.Errorf("mgr の LLM 呼び出し回数 = %d, want 1", got)
	}
}

func waitCalls(t *testing.T, c *scriptedClient, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Calls() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("LLM 呼び出しが %d 回に達しませんでした (calls=%d)", n, c.Calls())
}
