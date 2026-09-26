package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// --- テスト用ヘルパ ---

type notice struct {
	channel string
	fromID  string
	text    string
}

type recordingNotifier struct {
	mu      sync.Mutex
	notices []notice
	states  map[string][]string
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{states: make(map[string][]string)}
}

func (n *recordingNotifier) Notify(_ context.Context, channel, fromID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notices = append(n.notices, notice{channel: channel, fromID: fromID, text: text})
	return nil
}

func (n *recordingNotifier) SetAgentState(employeeID, state string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.states[employeeID] = append(n.states[employeeID], state)
}

func (n *recordingNotifier) noticesSnapshot() []notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notice(nil), n.notices...)
}

func (n *recordingNotifier) statesFor(id string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.states[id]...)
}

// syncBuffer は slog 出力を goroutine 安全に集める。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stepClient はテスト用の llm.Client。text が空なら呼び出しごとに一意な応答を返す。
type stepClient struct {
	mu   sync.Mutex
	n    int
	text string
	stop string
	in   int
	out  int
}

func (c *stepClient) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	text := c.text
	if text == "" {
		text = fmt.Sprintf("計画-%d", c.n)
	}
	return llm.Response{
		Text:         text,
		Model:        "step",
		InputTokens:  c.in,
		OutputTokens: c.out,
		StopReason:   c.stop,
	}, nil
}

// flakyClient は最初の呼び出しだけエラーを返す。エラー後も Run が継続するかの検証用。
type flakyClient struct {
	mu sync.Mutex
	n  int
}

func (c *flakyClient) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.n == 1 {
		return llm.Response{}, errors.New("一時的な失敗")
	}
	return llm.Response{Text: "計画B", Model: "flaky", StopReason: "end_turn"}, nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func containsSubsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// --- テスト本体 ---

func TestManagerPlansAndReports(t *testing.T) {
	notifier := newRecordingNotifier()
	client := llm.NewMock("計画: 1. 要件確認 2. 実装 3. レビュー")
	mgr := NewManager("mgr", persona.Persona{Name: "mgr", Role: "管理職"}, client, notifier, Options{Channel: "#会議室"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// (a) 起動時のチェックイン通知。
	if !waitFor(t, time.Second, func() bool { return len(notifier.noticesSnapshot()) >= 1 }) {
		t.Fatal("起動時の通知が投稿されませんでした")
	}

	// タスクを投入。
	if !mgr.Post(Task{ID: "t1", Title: "設計", Description: "API 設計をまとめる", From: "owner"}) {
		t.Fatal("Post が false を返しました（受信箱が満杯？）")
	}

	// 計画が報告されるまで待つ。
	gotNotice := waitFor(t, 2*time.Second, func() bool {
		for _, n := range notifier.noticesSnapshot() {
			if strings.Contains(n.text, "設計") && strings.Contains(n.text, "計画") {
				return true
			}
		}
		return false
	})
	if !gotNotice {
		t.Fatalf("タスクの計画が通知されませんでした: %+v", notifier.noticesSnapshot())
	}

	// (b) 状態遷移 idle -> thinking -> working -> idle を検証。
	states := notifier.statesFor("mgr")
	want := []string{StateIdle, StateThinking, StateWorking, StateIdle}
	if !containsSubsequence(states, want) {
		t.Fatalf("状態遷移が期待通りではありません: got=%v want(subseq)=%v", states, want)
	}

	// LLM に渡したリクエストの検証。
	reqs := client.Requests()
	if len(reqs) != 1 {
		t.Fatalf("LLM 呼び出し回数 = %d, want 1", len(reqs))
	}
	if reqs[0].Model == "" {
		t.Error("Model が空です")
	}
	if !strings.Contains(reqs[0].System, "管理職") {
		t.Errorf("システムプロンプトに計画指示が含まれていません: %q", reqs[0].System)
	}
	if !strings.Contains(reqs[0].Messages[0].Content, "設計") {
		t.Errorf("ユーザーメッセージにタスクが含まれていません: %q", reqs[0].Messages[0].Content)
	}
}

func TestManagerSurvivesLLMErrorAndContinues(t *testing.T) {
	notifier := newRecordingNotifier()
	client := &flakyClient{}
	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, client, notifier, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	// 1 件目は LLM がエラーを返すが、Run は落ちずに次のタスクへ進む。
	mgr.Post(Task{ID: "bad", Title: "失敗タスク", From: "owner"})
	mgr.Post(Task{ID: "good", Title: "成功タスク", From: "owner"})

	ok := waitFor(t, 2*time.Second, func() bool {
		for _, n := range notifier.noticesSnapshot() {
			if strings.Contains(n.text, "成功タスク") && strings.Contains(n.text, "計画B") {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("エラー後のタスクが処理されませんでした: %+v", notifier.noticesSnapshot())
	}

	// エラーは記録されているがクラッシュせず idle に戻っている。
	if got := mgr.State(); got != StateIdle {
		t.Errorf("State() = %q, want %q", got, StateIdle)
	}
	// エラータスクは報告されない（計画がないため）。
	for _, n := range notifier.noticesSnapshot() {
		if strings.Contains(n.text, "失敗タスク") {
			t.Errorf("エラーになったタスクが報告されています: %q", n.text)
		}
	}
}

func TestManagerInfiniteLoopGuards(t *testing.T) {
	tests := []struct {
		name   string
		client llm.Client
		opts   Options
		want   string
	}{
		{
			name:   "repeated_conclusion",
			client: &stepClient{text: "同じ結論", stop: "max_tokens", in: 1, out: 1},
			opts:   Options{MaxTurns: 10, TokenBudget: 1_000_000},
			want:   "repeated_conclusion",
		},
		{
			name:   "token_budget",
			client: &stepClient{stop: "max_tokens", in: 60, out: 60},
			opts:   Options{MaxTurns: 10, TokenBudget: 100},
			want:   "token_budget",
		},
		{
			name:   "max_turns",
			client: &stepClient{stop: "max_tokens", in: 1, out: 1},
			opts:   Options{MaxTurns: 2, TokenBudget: 1_000_000},
			want:   "max_turns",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logBuf := &syncBuffer{}
			tc.opts.Logger = slog.New(slog.NewTextHandler(logBuf, nil))

			notifier := newRecordingNotifier()
			mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, tc.client, notifier, tc.opts)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go mgr.Run(ctx)

			mgr.Post(Task{ID: "t", Title: "ガード確認", From: "owner"})

			if !waitFor(t, 2*time.Second, func() bool {
				return strings.Contains(logBuf.String(), tc.want)
			}) {
				t.Fatalf("ガード %q がログに記録されませんでした:\n%s", tc.want, logBuf.String())
			}

			// ガード作動後も idle に戻り、Run は生き続ける。
			if !waitFor(t, time.Second, func() bool { return mgr.State() == StateIdle }) {
				t.Errorf("ガード後も idle に戻りませんでした: %q", mgr.State())
			}
			if len(notifier.noticesSnapshot()) == 0 {
				t.Error("ガード作動時も最後の計画を報告すべきです")
			}
		})
	}
}

func TestManagerPostFullInbox(t *testing.T) {
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), newRecordingNotifier(), Options{})

	// Run していないので受信箱は消費されない。
	for i := 0; i < inboxCapacity; i++ {
		if !mgr.Post(Task{ID: fmt.Sprintf("t%d", i)}) {
			t.Fatalf("Post %d 回目が false を返しました", i+1)
		}
	}
	if mgr.Post(Task{ID: "overflow"}) {
		t.Fatal("満杯の受信箱への Post は false を返すべきです")
	}
}

func TestManagerPostClosedInbox(t *testing.T) {
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), newRecordingNotifier(), Options{})
	close(mgr.Inbox())
	if mgr.Post(Task{ID: "t"}) {
		t.Fatal("クローズ済みの受信箱への Post は false を返すべきです")
	}
}

func TestManagerNilClientDoesNotCrash(t *testing.T) {
	notifier := newRecordingNotifier()
	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, nil, notifier, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	mgr.Post(Task{ID: "t", Title: "タスク", From: "owner"})

	if !waitFor(t, time.Second, func() bool { return mgr.State() == StateIdle }) {
		t.Fatalf("nil client でも idle に戻るべきです: %q", mgr.State())
	}
	// 起動通知のみで、タスクの計画は投稿されない。
	for _, n := range notifier.noticesSnapshot() {
		if strings.Contains(n.text, "タスク") && strings.Contains(n.text, "【") {
			t.Errorf("nil client で計画が投稿されました: %q", n.text)
		}
	}
}

func TestManagerSystemPromptFallback(t *testing.T) {
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	if got := mgr.systemPrompt(); got != planningInstruction {
		t.Errorf("ペルソナ無しの場合の systemPrompt = %q, want %q", got, planningInstruction)
	}

	mgr2 := NewManager("mgr", persona.Persona{System: "独自プロンプト"}, llm.NewMock(), nil, Options{})
	if got := mgr2.systemPrompt(); !strings.HasPrefix(got, "独自プロンプト") || !strings.Contains(got, planningInstruction) {
		t.Errorf("独自プロンプトに計画指示が付加されていません: %q", got)
	}
}
