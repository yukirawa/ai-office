package agents

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// --- テスト用フェイク（§16 エスカレーション） ---

// ownerRecorder は Notifier と OwnerChannel の両方を実装する。
// AskOwner で受け取った質問を記録し、Escalate 側からの問い合わせに使う。
type ownerRecorder struct {
	mu        sync.Mutex
	questions []Question
}

func (o *ownerRecorder) Notify(context.Context, string, string, string) error { return nil }

func (o *ownerRecorder) SetAgentState(string, string) {}

func (o *ownerRecorder) AskOwner(_ context.Context, q Question) error {
	o.mu.Lock()
	o.questions = append(o.questions, q)
	o.mu.Unlock()
	return nil
}

func (o *ownerRecorder) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.questions)
}

func (o *ownerRecorder) lastQuestion() (Question, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.questions) == 0 {
		return Question{}, false
	}
	return o.questions[len(o.questions)-1], true
}

// fakeEscalator は Escalator 実装。Escalate で受け取った質問を記録し、
// あらかじめ設定した回答を返す（回答なしにしたい場合は ok=false にする）。
type fakeEscalator struct {
	mu        sync.Mutex
	questions []Question
	answer    string
	ok        bool
}

func (f *fakeEscalator) Escalate(_ context.Context, q Question) (string, bool) {
	f.mu.Lock()
	f.questions = append(f.questions, q)
	f.mu.Unlock()
	return f.answer, f.ok
}

func (f *fakeEscalator) questionsSnapshot() []Question {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Question(nil), f.questions...)
}

// --- テスト本体 ---

// Escalate はオーナーへ質問を上げ、Answer で渡された回答を返す。
func TestManagerEscalateDeliversAnswer(t *testing.T) {
	owner := &ownerRecorder{}
	mgr := NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, owner, Options{AnswerTimeout: 3 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type escResult struct {
		answer string
		ok     bool
	}
	resultCh := make(chan escResult, 1)
	go func() {
		a, ok := mgr.Escalate(ctx, Question{FromID: "dev_m", TaskID: "t1", Text: "どうしますか"})
		resultCh <- escResult{answer: a, ok: ok}
	}()

	// オーナーへ質問が届くまで待ち、採番された ID を得る。
	if !waitFor(t, 2*time.Second, func() bool { return owner.count() == 1 }) {
		t.Fatal("オーナーへの質問が届きませんでした")
	}
	q, _ := owner.lastQuestion()
	if q.ID == "" {
		t.Fatal("質問 ID が採番されていません")
	}
	if q.FromID != "dev_m" || q.TaskID != "t1" || q.Text != "どうしますか" {
		t.Errorf("質問の内容が不正です: %+v", q)
	}

	// 待機中の質問へ回答を渡す。
	if !mgr.Answer(q.ID, "これでお願いします") {
		t.Fatal("Answer が false を返しました（待機中の質問が見つからない）")
	}

	select {
	case got := <-resultCh:
		if got.answer != "これでお願いします" || !got.ok {
			t.Errorf("Escalate = (%q, %v), want (\"これでお願いします\", true)", got.answer, got.ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Escalate が回答を受け取れませんでした")
	}
}

// OwnerChannel を実装しない Notifier しか無い場合は、速やかに ok=false を返す。
func TestManagerEscalateNoOwnerChannel(t *testing.T) {
	notifier := newRecordingNotifier()
	mgr := NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, notifier, Options{AnswerTimeout: 3 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	answer, ok := mgr.Escalate(ctx, Question{FromID: "dev_m", TaskID: "t1", Text: "どうしますか"})
	if ok || answer != "" {
		t.Fatalf("Escalate = (%q, %v), want (\"\", false)", answer, ok)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("オーナー経路が無い場合は速やかに戻るべきです: %v", elapsed)
	}
}

// 回答が来ない場合は AnswerTimeout で ok=false を返す。
func TestManagerEscalateTimesOut(t *testing.T) {
	owner := &ownerRecorder{}
	mgr := NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, owner, Options{AnswerTimeout: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	answer, ok := mgr.Escalate(ctx, Question{FromID: "dev_m", TaskID: "t1", Text: "どうしますか"})
	elapsed := time.Since(start)

	if ok || answer != "" {
		t.Fatalf("Escalate = (%q, %v), want (\"\", false)", answer, ok)
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("AnswerTimeout より早く戻りすぎています: %v", elapsed)
	}
	if owner.count() != 1 {
		t.Errorf("オーナーへの質問回数 = %d, want 1", owner.count())
	}
}

// nextAssignee は計画文で名指しされた dev を優先し、名指しが無ければ負荷を均す。
func TestNextAssigneePrefersMentionedAndBalancesLoad(t *testing.T) {
	devM := NewDevAgent("dev_m", persona.Persona{Name: "タクミ"}, nil, nil, nil, nil, nil, nil, Options{})
	devF := NewDevAgent("dev_f", persona.Persona{Name: "ミナ"}, nil, nil, nil, nil, nil, nil, Options{})
	mgr := NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, nil, Options{})
	mgr.SetAssignees(devM, devF)

	// 計画文で dev_f が名指しされている場合は dev_f を選ぶ。
	if got := mgr.nextAssignee("担当: dev_f でお願いします"); got == nil || got.ID() != "dev_f" {
		t.Fatalf("名指しされた dev が選ばれていません: got=%v, want dev_f", got)
	}

	// 負荷分散は担当件数を初期化してから検証する（名指しの 1 件分を除くため）。
	mgr.SetAssignees(devM, devF)
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		a := mgr.nextAssignee("進めます")
		if a == nil {
			t.Fatal("nextAssignee が nil を返しました")
		}
		counts[a.ID()]++
	}
	if counts["dev_m"] != 2 || counts["dev_f"] != 2 {
		t.Fatalf("負荷が偏っています: %v, want dev_m=2 dev_f=2", counts)
	}
}

// dev は LLM が question を返すと mgr 経由でオーナーへ上げ、
// 回答を踏まえて計画をやり直し、その actions でディスパッチする。
func TestDevAsksOwnerThenReplans(t *testing.T) {
	notifier := newRecordingNotifier()
	dispatcher := &fakeDispatcher{awaitResult: Result{TaskID: "t1", Status: "done", Summary: "完了しました"}}
	// 1 回目は質問、2 回目は回答を踏まえた actions を返す。
	client := llm.NewMock(
		`{"question":"色は何にしますか"}`,
		`{"actions":[{"op":"write","path":"x.txt","content":"red"}]}`,
	)
	dev := NewDevAgent("dev_m", persona.Persona{Name: "タクミ"}, client, notifier,
		dispatcher, nil, nil, nil, Options{})

	esc := &fakeEscalator{answer: "赤で", ok: true}
	dev.SetEscalator(esc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dev.runTask(ctx, Task{ID: "t1", Title: "x"})

	qs := esc.questionsSnapshot()
	if len(qs) != 1 {
		t.Fatalf("エスカレーション回数 = %d, want 1", len(qs))
	}
	if qs[0].Text != "色は何にしますか" {
		t.Errorf("質問本文 = %q, want 色は何にしますか", qs[0].Text)
	}
	if qs[0].FromID != "dev_m" || qs[0].TaskID != "t1" {
		t.Errorf("質問の送信元/タスクが不正です: %+v", qs[0])
	}

	assign := dispatcher.last()
	if len(assign.Actions) != 1 {
		t.Fatalf("Actions = %+v, want 1 件", assign.Actions)
	}
	if assign.Actions[0].Content != "red" {
		t.Errorf("Actions[0].Content = %q, want red（回答を踏まえた再計画）", assign.Actions[0].Content)
	}
}
