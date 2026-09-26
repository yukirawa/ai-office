package agents

import (
	"context"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// llmOptionCase は Options の Model / MaxTokens がリクエストへ伝播するかの検証ケース。
type llmOptionCase struct {
	name      string
	opts      Options
	wantModel string
	wantMax   int
}

// llmOptionCases は「明示指定」と「ゼロ値フォールバック」の 2 ケースを返す。
func llmOptionCases() []llmOptionCase {
	return []llmOptionCase{
		{
			name:      "explicit",
			opts:      Options{Model: "test-model", MaxTokens: 64},
			wantModel: "test-model",
			wantMax:   64,
		},
		{
			name:      "fallback",
			opts:      Options{},
			wantModel: DefaultModel,
			wantMax:   defaultMaxTokens,
		},
	}
}

// TestManagerUsesOptionsModelAndMaxTokens は mgr の計画リクエストに
// Options.Model / Options.MaxTokens が反映されることを検証する。
func TestManagerUsesOptionsModelAndMaxTokens(t *testing.T) {
	for _, tc := range llmOptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			client := llm.NewMock("計画: テスト")
			mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, client, notifier, tc.opts)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go mgr.Run(ctx)

			if !mgr.Post(Task{ID: "t1", Title: "確認", Description: "モデル確認", From: "owner"}) {
				t.Fatal("Post が false を返しました")
			}
			if !waitFor(t, 2*time.Second, func() bool { return len(client.Requests()) >= 1 }) {
				t.Fatal("LLM が呼び出されませんでした")
			}

			reqs := client.Requests()
			if got := reqs[0].Model; got != tc.wantModel {
				t.Errorf("req.Model = %q, want %q", got, tc.wantModel)
			}
			if got := reqs[0].MaxTokens; got != tc.wantMax {
				t.Errorf("req.MaxTokens = %d, want %d", got, tc.wantMax)
			}
		})
	}
}

// TestDevAgentUsesOptionsModelAndMaxTokens は dev の計画リクエストに
// Options.Model / Options.MaxTokens が反映されることを検証する。
func TestDevAgentUsesOptionsModelAndMaxTokens(t *testing.T) {
	for _, tc := range llmOptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			dispatcher := &fakeDispatcher{awaitResult: Result{TaskID: "t1", Status: "done", Summary: "完了"}}
			reviewer := &fakeReviewer{}
			client := llm.NewMock("散文のみ（JSON ではない）")

			dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
				dispatcher, nil, nil, reviewer, tc.opts)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go dev.Run(ctx)

			dev.Post(Task{ID: "t1", Title: "実装", Plan: "計画"})
			if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
				t.Fatal("レビューが呼ばれませんでした")
			}

			reqs := client.Requests()
			if len(reqs) == 0 {
				t.Fatal("LLM が呼び出されませんでした")
			}
			if got := reqs[0].Model; got != tc.wantModel {
				t.Errorf("req.Model = %q, want %q", got, tc.wantModel)
			}
			if got := reqs[0].MaxTokens; got != tc.wantMax {
				t.Errorf("req.MaxTokens = %d, want %d", got, tc.wantMax)
			}
		})
	}
}

// TestChatAgentUsesOptionsModelAndMaxTokens は chat の応答リクエストに
// Options.Model / Options.MaxTokens が反映されることを検証する。
func TestChatAgentUsesOptionsModelAndMaxTokens(t *testing.T) {
	for _, tc := range llmOptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			client := llm.NewMock("こんにちは！")
			chat := NewChatAgent("chat", persona.Persona{Name: "チャット"}, client, notifier, tc.opts)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go chat.Run(ctx)

			if !chat.Post("やあ") {
				t.Fatal("Post が false を返しました")
			}
			if !waitFor(t, 2*time.Second, func() bool { return len(client.Requests()) >= 1 }) {
				t.Fatal("LLM が呼び出されませんでした")
			}

			reqs := client.Requests()
			if got := reqs[0].Model; got != tc.wantModel {
				t.Errorf("req.Model = %q, want %q", got, tc.wantModel)
			}
			if got := reqs[0].MaxTokens; got != tc.wantMax {
				t.Errorf("req.MaxTokens = %d, want %d", got, tc.wantMax)
			}
		})
	}
}
