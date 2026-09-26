package agents

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// --- テスト用フェイク ---

type fakeDispatcher struct {
	mu          sync.Mutex
	assigns     []TaskAssign
	awaitTasks  []string
	dispatchErr error
	awaitResult Result
	awaitErr    error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ string, a TaskAssign) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assigns = append(f.assigns, a)
	return f.dispatchErr
}

func (f *fakeDispatcher) Await(_ context.Context, taskID string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awaitTasks = append(f.awaitTasks, taskID)
	return f.awaitResult, f.awaitErr
}

func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.assigns)
}

func (f *fakeDispatcher) last() TaskAssign {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.assigns) == 0 {
		return TaskAssign{}
	}
	return f.assigns[len(f.assigns)-1]
}

type statusCall struct {
	taskID string
	status string
	result string
}

type fakeUpdater struct {
	mu    sync.Mutex
	calls []statusCall
}

func (u *fakeUpdater) UpdateTask(_ context.Context, taskID, status, result string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, statusCall{taskID: taskID, status: status, result: result})
	return nil
}

func (u *fakeUpdater) statuses(taskID string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, c := range u.calls {
		if c.taskID == taskID {
			out = append(out, c.status)
		}
	}
	return out
}

func (u *fakeUpdater) has(taskID, status string) bool {
	for _, s := range u.statuses(taskID) {
		if s == status {
			return true
		}
	}
	return false
}

type fakeRemote struct {
	mu      sync.Mutex
	specs   []RemoteSpec
	result  Result
	err     error
	started chan struct{}
}

func (r *fakeRemote) ExecuteRemote(_ context.Context, spec RemoteSpec) (Result, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	return r.result, r.err
}

func (r *fakeRemote) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.specs)
}

type reviewCall struct {
	task Task
	res  Result
}

type fakeReviewer struct {
	mu      sync.Mutex
	reviews []reviewCall
}

func (r *fakeReviewer) Review(_ context.Context, t Task, res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reviews = append(r.reviews, reviewCall{task: t, res: res})
}

func (r *fakeReviewer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reviews)
}

func (r *fakeReviewer) last() reviewCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reviews) == 0 {
		return reviewCall{}
	}
	return r.reviews[len(r.reviews)-1]
}

// recvTask は受信箱からタスクを 1 件受け取る（テスト用）。
func recvTask(t *testing.T, ch chan Task) Task {
	t.Helper()
	select {
	case tk := <-ch:
		return tk
	case <-time.After(2 * time.Second):
		t.Fatal("タスクを受信できませんでした")
		return Task{}
	}
}

func noticeContains(notifier *recordingNotifier, substr string) bool {
	for _, n := range notifier.noticesSnapshot() {
		if strings.Contains(n.text, substr) {
			return true
		}
	}
	return false
}

// --- テスト本体 ---

// local タスクは mode=local で reports/<id>.md への write を含む task_assign を生成し、
// mock が散文を返す場合はフォールバックが使われる。状態遷移 working→review と、
// mgr(Reviewer) による done への遷移も検証する。
func TestDevAgentLocalFallbackAndReview(t *testing.T) {
	notifier := newRecordingNotifier()
	dispatcher := &fakeDispatcher{awaitResult: Result{TaskID: "task-1", Status: "done", Summary: "完了しました"}}
	updater := &fakeUpdater{}

	// 実物の Manager を Reviewer として使う（Review が done に更新することを検証）。
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), notifier, Options{})
	mgr.SetTaskUpdater(updater)

	// mock は既定で散文を返すため JSON 解析は失敗し、フォールバックが使われる。
	dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, llm.NewMock(), notifier,
		dispatcher, updater, nil, mgr, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	if !dev.Post(Task{ID: "task-1", Title: "実装", Description: "説明文", Plan: "計画: 直す", From: "mgr"}) {
		t.Fatal("Post が false を返しました")
	}

	// Review により done へ遷移するまで待つ。
	if !waitFor(t, 2*time.Second, func() bool { return updater.has("task-1", "done") }) {
		t.Fatalf("Review が done にしませんでした: %+v", updater.statuses("task-1"))
	}

	assign := dispatcher.last()
	if assign.Mode != "local" {
		t.Errorf("assign.Mode = %q, want local", assign.Mode)
	}
	if assign.Reason != "計画: 直す" {
		t.Errorf("assign.Reason = %q, want mgr の計画", assign.Reason)
	}
	if len(assign.Actions) == 0 {
		t.Fatalf("assign.Actions が空です")
	}
	first := assign.Actions[0]
	if first.Op != "write" {
		t.Errorf("Actions[0].Op = %q, want write", first.Op)
	}
	if first.Path != "reports/task-1.md" {
		t.Errorf("Actions[0].Path = %q, want reports/task-1.md", first.Path)
	}
	for _, want := range []string{"実装", "説明文", "計画: 直す"} {
		if !strings.Contains(first.Content, want) {
			t.Errorf("フォールバックレポートに %q が含まれていません: %q", want, first.Content)
		}
	}

	// 状態遷移: idle(起動) → thinking → working → idle、updater は working → review → done。
	if got := updater.statuses("task-1"); !containsSubsequence(got, []string{"working", "review", "done"}) {
		t.Errorf("updater の状態遷移 = %v, want subseq [working review done]", got)
	}
	if got := notifier.statesFor("dev"); !containsSubsequence(got, []string{StateIdle, StateThinking, StateWorking, StateIdle}) {
		t.Errorf("dev の状態遷移 = %v, want subseq [idle thinking working idle]", got)
	}
	if !noticeContains(notifier, "【レビュー】") {
		t.Errorf("レビュー通知が投稿されていません: %+v", notifier.noticesSnapshot())
	}
	if got := dev.State(); got != StateIdle {
		t.Errorf("dev.State() = %q, want idle", got)
	}
}

// LLM が有効な JSON を返した場合はその actions をそのまま使う。
func TestDevAgentLocalLLMActions(t *testing.T) {
	notifier := newRecordingNotifier()
	dispatcher := &fakeDispatcher{awaitResult: Result{TaskID: "t1", Status: "done", Summary: "ok"}}
	reviewer := &fakeReviewer{}

	jsonResp := "```json\n{\"actions\":[{\"op\":\"write\",\"path\":\"src/main.go\",\"content\":\"package main\"}]}\n```"
	dev := NewDevAgent("dev", persona.Persona{}, llm.NewMock(jsonResp), notifier,
		dispatcher, nil, nil, reviewer, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	dev.Post(Task{ID: "t1", Title: "整形", Plan: "計画"})

	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("レビューが呼ばれませんでした")
	}
	assign := dispatcher.last()
	if assign.Mode != "local" {
		t.Fatalf("assign.Mode = %q, want local", assign.Mode)
	}
	if len(assign.Actions) != 1 || assign.Actions[0].Path != "src/main.go" {
		t.Fatalf("LLM の actions が使われていません: %+v", assign.Actions)
	}
	if assign.Actions[0].Content != "package main" {
		t.Errorf("content が保持されていません: %q", assign.Actions[0].Content)
	}
	// システムプロンプトに厳密 JSON の要求が含まれる。
	reqs := mustMockRequests(t, dev)
	if len(reqs) > 0 && !strings.Contains(reqs[0].System, "JSON") {
		t.Errorf("システムプロンプトに JSON 指示がありません: %q", reqs[0].System)
	}
}

// remote タスクで dispatch が失敗したら RemoteExecutor にフォールバックする。
func TestDevAgentRemoteFallbackOnDispatchError(t *testing.T) {
	notifier := newRecordingNotifier()
	dispatcher := &fakeDispatcher{dispatchErr: errors.New("worker offline")}
	updater := &fakeUpdater{}
	remote := &fakeRemote{result: Result{Status: "done", Summary: "PR を作成しました"}}
	reviewer := &fakeReviewer{}

	dev := NewDevAgent("dev", persona.Persona{}, llm.NewMock(), notifier,
		dispatcher, updater, remote, reviewer, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	dev.Post(Task{ID: "r1", Title: "修正", Mode: "remote", Repo: "owner/name", Plan: "計画"})

	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("レビューが呼ばれませんでした")
	}
	if remote.count() != 1 {
		t.Fatalf("RemoteExecutor の呼び出し回数 = %d, want 1", remote.count())
	}

	assign := dispatcher.last()
	if assign.Mode != "remote" {
		t.Fatalf("assign.Mode = %q, want remote", assign.Mode)
	}
	if assign.Remote == nil {
		t.Fatal("assign.Remote が nil です")
	}
	if assign.Remote.Repo != "owner/name" {
		t.Errorf("Repo = %q, want owner/name", assign.Remote.Repo)
	}
	if assign.Remote.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main（既定）", assign.Remote.BaseBranch)
	}
	if assign.Remote.Branch != "ai-office/task-r1" {
		t.Errorf("Branch = %q, want ai-office/task-r1", assign.Remote.Branch)
	}
	if assign.Remote.Token != "" {
		t.Errorf("Token は api が埋めるため空であるべきです: %q", assign.Remote.Token)
	}
	if len(assign.Remote.Files) == 0 || assign.Remote.Files[0].Path != "reports/r1.md" {
		t.Fatalf("remote のフォールバック files が不正です: %+v", assign.Remote.Files)
	}

	got := reviewer.last().res
	if got.Status != "done" || got.Summary != "PR を作成しました" {
		t.Errorf("review に渡った Result = %+v, want remote の結果", got)
	}
}

// local タスクで dispatch が失敗したら failed になり、Run は落ちずに次のタスクを処理する。
func TestDevAgentLocalDispatchErrorSurvives(t *testing.T) {
	notifier := newRecordingNotifier()
	dispatcher := &fakeDispatcher{dispatchErr: errors.New("boom")}
	reviewer := &fakeReviewer{}

	dev := NewDevAgent("dev", persona.Persona{}, llm.NewMock(), notifier,
		dispatcher, nil, nil, reviewer, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	dev.Post(Task{ID: "f1", Title: "失敗タスク"})
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
		t.Fatal("1 件目のレビューが呼ばれませんでした")
	}
	first := reviewer.last().res
	if first.Status != "failed" {
		t.Errorf("Status = %q, want failed", first.Status)
	}
	if !strings.Contains(first.Summary, "実行できませんでした") {
		t.Errorf("Summary = %q, want 実行できませんでした を含む", first.Summary)
	}

	// 2 件目も処理され、Run が生き続けていることを確認。
	dev.Post(Task{ID: "f2", Title: "次のタスク"})
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 2 }) {
		t.Fatal("dispatch エラー後に Run が停止しました")
	}
	if got := dev.State(); got != StateIdle {
		t.Errorf("dev.State() = %q, want idle", got)
	}
	if !noticeContains(notifier, "失敗タスク") {
		t.Errorf("結果通知が投稿されていません: %+v", notifier.noticesSnapshot())
	}
}

// mgr は 2 体の dev にラウンドロビンで割り当て、計画をチャンネルへ投稿する。
func TestManagerRoundRobinAssignees(t *testing.T) {
	notifier := newRecordingNotifier()
	updater := &fakeUpdater{}
	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, llm.NewMock("計画A", "計画B", "計画C"), notifier, Options{})
	mgr.SetTaskUpdater(updater)

	// dev は Run させず、受信箱に積まれたタスクを直接読んで割当を検証する。
	d1 := NewDevAgent("dev1", persona.Persona{}, llm.NewMock(), notifier, nil, nil, nil, nil, Options{})
	d2 := NewDevAgent("dev2", persona.Persona{}, llm.NewMock(), notifier, nil, nil, nil, nil, Options{})
	mgr.SetAssignees(d1, d2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	mgr.Post(Task{ID: "t1", Title: "A", From: "owner"})
	mgr.Post(Task{ID: "t2", Title: "B", From: "owner"})
	mgr.Post(Task{ID: "t3", Title: "C", From: "owner"})

	got1 := recvTask(t, d1.inbox)
	got2 := recvTask(t, d2.inbox)
	got3 := recvTask(t, d1.inbox)

	if got1.ID != "t1" || got2.ID != "t2" || got3.ID != "t3" {
		t.Fatalf("ラウンドロビン割当が不正です: d1=%s d2=%s d1=%s", got1.ID, got2.ID, got3.ID)
	}
	if got1.Plan != "計画A" || got3.Plan != "計画C" {
		t.Errorf("計画が Task に渡っていません: t1=%q t3=%q", got1.Plan, got3.Plan)
	}

	// 3 件とも assigned に更新されている。
	for _, id := range []string{"t1", "t2", "t3"} {
		if !waitFor(t, time.Second, func() bool { return updater.has(id, "assigned") }) {
			t.Errorf("タスク %s が assigned に更新されていません: %v", id, updater.statuses(id))
		}
	}
	// 計画がチャンネルへ投稿されている。
	if !waitFor(t, time.Second, func() bool { return noticeContains(notifier, "計画A") }) {
		t.Errorf("計画がチャンネルへ投稿されていません: %+v", notifier.noticesSnapshot())
	}
	if noticeContains(notifier, "担当者が割り当てられていません") {
		t.Error("担当者がいるのに未割当の通知が出ています")
	}
}

// 担当者未設定の場合はタスクを failed にする。
func TestManagerNoAssigneeMarksFailed(t *testing.T) {
	notifier := newRecordingNotifier()
	updater := &fakeUpdater{}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock("計画"), notifier, Options{})
	mgr.SetTaskUpdater(updater)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	mgr.Post(Task{ID: "t1", Title: "未割当", From: "owner"})

	if !waitFor(t, 2*time.Second, func() bool { return updater.has("t1", "failed") }) {
		t.Fatalf("未割当タスクが failed になっていません: %v", updater.statuses("t1"))
	}
	if !noticeContains(notifier, "担当者が割り当てられていません") {
		t.Errorf("未割当の通知がありません: %+v", notifier.noticesSnapshot())
	}
	if got := mgr.State(); got != StateIdle {
		t.Errorf("mgr.State() = %q, want idle", got)
	}
}

// mustMockRequests は DevAgent が持つ MockClient の記録を返す。
// テストでは client を llm.NewMock で作っているため型アサートできる。
func mustMockRequests(t *testing.T, d *DevAgent) []llm.Request {
	t.Helper()
	if mc, ok := d.client.(*llm.MockClient); ok {
		return mc.Requests()
	}
	return nil
}
