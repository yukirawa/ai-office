package agents

// loop_test.go は以下 2 つの実装済み機能の回帰テストをまとめる。
//
// - DevAgent の反復ループ（localLoop）: 「LLM に次の一手を聞く → 実行 → 結果を渡す」を繰り返す
// - 自発活動（Initiative / normalizeInitiative）: idle のときだけ自発発言テキストを返す
//
// 既存テストのフェイク（fakeDispatcher / fakeReviewer / fakeEscalator /
// newRecordingNotifier / waitFor / mustMockRequests）を再利用する。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// TestDevLocalLoopRunsActionsThenDone は local タスクで
// 「actions を実行 → done で完了」の 2 ラウンドが回ることを検証する。
func TestDevLocalLoopRunsActionsThenDone(t *testing.T) {
	notifier := newRecordingNotifier()
	// 1 ラウンド目は actions、2 ラウンド目で done を返す。
	client := llm.NewMock(
		`{"actions":[{"op":"write","path":"a.txt","content":"x"}]}`,
		`{"done":true,"summary":"完了しました"}`,
	)
	dispatcher := &fakeDispatcher{awaitResult: Result{Status: "done", Summary: "a.txt を書きました"}}
	reviewer := &fakeReviewer{}

	dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
		dispatcher, nil, nil, reviewer, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	if !dev.Post(Task{ID: "t1", Title: "x"}) {
		t.Fatal("Post が false を返しました")
	}
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("レビューが呼ばれませんでした")
	}

	// LLM が返した actions がそのまま dispatcher に届いている。
	assign := dispatcher.last()
	if len(assign.Actions) == 0 {
		t.Fatalf("dispatcher に actions が届いていません: %+v", assign)
	}
	if assign.Actions[0].Path != "a.txt" {
		t.Errorf("Actions[0].Path = %q, want a.txt", assign.Actions[0].Path)
	}
	if assign.Actions[0].Op != "write" {
		t.Errorf("Actions[0].Op = %q, want write", assign.Actions[0].Op)
	}

	// done の summary が完了要約として reviewer に渡る（worker の結果要約ではない）。
	got := reviewer.last().res
	if got.Status != "done" {
		t.Errorf("review に渡った Status = %q, want done", got.Status)
	}
	if got.Summary != "完了しました" {
		t.Errorf("review に渡った Summary = %q, want 完了しました", got.Summary)
	}
}

// TestDevLocalLoopFeedsObservationToNextTurn は 1 ラウンド目の実行結果が
// 観察ログとして 2 ラウンド目の LLM プロンプトに引き継がれることを検証する。
func TestDevLocalLoopFeedsObservationToNextTurn(t *testing.T) {
	notifier := newRecordingNotifier()
	client := llm.NewMock(
		`{"actions":[{"op":"write","path":"a.txt","content":"x"}]}`,
		`{"done":true,"summary":"完了しました"}`,
	)
	dispatcher := &fakeDispatcher{awaitResult: Result{Status: "done", Summary: "a.txt を書きました"}}
	reviewer := &fakeReviewer{}

	dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
		dispatcher, nil, nil, reviewer, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	if !dev.Post(Task{ID: "t1", Title: "x"}) {
		t.Fatal("Post が false を返しました")
	}
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("レビューが呼ばれませんでした")
	}

	reqs := mustMockRequests(t, dev)
	if len(reqs) < 2 {
		t.Fatalf("LLM 呼び出し回数 = %d, want >= 2", len(reqs))
	}
	second := reqs[1]
	if len(second.Messages) == 0 {
		t.Fatal("2 回目のリクエストにメッセージがありません")
	}
	content := second.Messages[0].Content
	if !strings.Contains(content, "これまでの実行結果") {
		t.Errorf("2 回目のプロンプトに観察ログの見出しがありません: %q", content)
	}
	if !strings.Contains(content, "a.txt を書きました") {
		t.Errorf("2 回目のプロンプトに worker の結果要約がありません: %q", content)
	}
}

// TestDevLocalLoopEscalatesQuestionThenContinues は LLM が question を返したとき、
// エスカレーションで得た回答を踏まえて次のラウンドへ進むことを検証する。
func TestDevLocalLoopEscalatesQuestionThenContinues(t *testing.T) {
	notifier := newRecordingNotifier()
	// 1 ラウンド目は question、2 ラウンド目はその回答を踏まえた actions、3 ラウンド目で done。
	client := llm.NewMock(
		`{"question":"色は？"}`,
		`{"actions":[{"op":"write","path":"c.txt","content":"red"}]}`,
		`{"done":true,"summary":"おわり"}`,
	)
	dispatcher := &fakeDispatcher{awaitResult: Result{Status: "done", Summary: "wrote"}}
	reviewer := &fakeReviewer{}

	dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
		dispatcher, nil, nil, reviewer, Options{})
	esc := &fakeEscalator{answer: "赤で", ok: true}
	dev.SetEscalator(esc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	if !dev.Post(Task{ID: "t1", Title: "x"}) {
		t.Fatal("Post が false を返しました")
	}
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("レビューが呼ばれませんでした")
	}

	// 質問の後に返した actions が dispatcher に届いている。
	assign := dispatcher.last()
	if len(assign.Actions) == 0 {
		t.Fatalf("dispatcher に actions が届いていません: %+v", assign)
	}
	if assign.Actions[0].Path != "c.txt" {
		t.Errorf("Actions[0].Path = %q, want c.txt", assign.Actions[0].Path)
	}
	if assign.Actions[0].Content != "red" {
		t.Errorf("Actions[0].Content = %q, want red", assign.Actions[0].Content)
	}

	// LLM が返した question がそのまま Escalate に渡っている。
	qs := esc.questionsSnapshot()
	if len(qs) != 1 {
		t.Fatalf("エスカレーション回数 = %d, want 1", len(qs))
	}
	if qs[0].Text != "色は？" {
		t.Errorf("Escalate に渡った Text = %q, want 色は？", qs[0].Text)
	}
}

// TestInitiativeReturnsTextOnlyWhenIdle は Initiative が idle のときだけ
// LLM を呼んでテキストを返し、それ以外の状態では空文字を返すことを検証する。
func TestInitiativeReturnsTextOnlyWhenIdle(t *testing.T) {
	notifier := newRecordingNotifier()
	client := llm.NewMock("@mgr 進行どう？")
	d := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
		nil, nil, nil, nil, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := d.Initiative(ctx, "在席: dev"); got != "@mgr 進行どう？" {
		t.Fatalf("idle の Initiative = %q, want @mgr 進行どう？", got)
	}

	// idle 以外では LLM を呼ばずに空文字を返す。
	d.setState(StateWorking)
	if got := d.Initiative(ctx, "在席: dev"); got != "" {
		t.Errorf("working の Initiative = %q, want 空文字", got)
	}

	reqs := mustMockRequests(t, d)
	if len(reqs) != 1 {
		t.Errorf("LLM 呼び出し回数 = %d, want 1（idle 以外では呼ばない）", len(reqs))
	}
}

// TestNormalizeInitiativeStripsEmptyReplies は「特に無し」を表す返答が
// 空文字へ正規化され、通常の発言はそのまま残ることを検証する。
func TestNormalizeInitiativeStripsEmptyReplies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空文字", "", ""},
		{"なし", "なし", ""},
		{"特になし", "特になし", ""},
		{"none", "none", ""},
		{"宛先付きの発言は保持", "@dev_m やあ", "@dev_m やあ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeInitiative(tc.in); got != tc.want {
				t.Errorf("normalizeInitiative(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
