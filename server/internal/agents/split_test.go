package agents

// split_test.go は「分担（サブタスク）」機能の回帰テストをまとめる。
//
// - 計画 LLM が JSON を返すと複数 dev へ分解・配信される（dispatchSubtasks）
// - Review が子タスクの完了状況を集約して親プロジェクトを done / failed にする
//   （aggregateProject / ParentTaskID / SubtaskStatuses）
//
// 既存テストのヘルパ（newRecordingNotifier / waitFor / recvTask / fakeUpdater /
// noticeContains）を再利用し、再定義しない。

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// fakeCoordinator は TaskCoordinator のテスト用実装。
// CreateSubtask は呼び出しを記録し sub-1, sub-2… を採番して返す。
// ParentTaskID / SubtaskStatuses はあらかじめ設定したマップを返す。
type fakeCoordinator struct {
	mu       sync.Mutex
	created  []Subtask
	parentOf map[string]string
	statuses map[string][]TaskStatus
}

func newFakeCoordinator() *fakeCoordinator {
	return &fakeCoordinator{
		parentOf: make(map[string]string),
		statuses: make(map[string][]TaskStatus),
	}
}

func (c *fakeCoordinator) CreateSubtask(_ context.Context, _ Task, sub Subtask) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, sub)
	return fmt.Sprintf("sub-%d", len(c.created)), nil
}

func (c *fakeCoordinator) SubtaskStatuses(_ context.Context, parentID string) ([]TaskStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]TaskStatus(nil), c.statuses[parentID]...), nil
}

func (c *fakeCoordinator) ParentTaskID(_ context.Context, taskID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parentOf[taskID], nil
}

// count は CreateSubtask の呼び出し回数を返す（goroutine 安全）。
func (c *fakeCoordinator) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.created)
}

// createdTitles は作成したサブタスクのタイトル一覧を返す。
func (c *fakeCoordinator) createdTitles() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.created))
	for _, s := range c.created {
		out = append(out, s.Title)
	}
	return out
}

// TestManagerSplitsTaskIntoSubtasks は計画 JSON が複数 dev の受信箱へ
// 分解配信され、親タスクが working になることを検証する。
func TestManagerSplitsTaskIntoSubtasks(t *testing.T) {
	notifier := newRecordingNotifier()
	updater := &fakeUpdater{}
	coord := newFakeCoordinator()

	// 計画 LLM は分担 JSON を返す。
	planJSON := `{"summary":"分担します","subtasks":[` +
		`{"title":"A","assignee":"dev_m"},{"title":"B","assignee":"dev_f"}]}`
	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, llm.NewMock(planJSON), notifier, Options{})
	mgr.SetTaskUpdater(updater)
	mgr.SetTaskCoordinator(coord)

	// dev は Run させず、受信箱に積まれた子タスクを直接読んで配信を検証する。
	devM := NewDevAgent("dev_m", persona.Persona{}, llm.NewMock(), notifier, nil, nil, nil, nil, Options{})
	devF := NewDevAgent("dev_f", persona.Persona{}, llm.NewMock(), notifier, nil, nil, nil, nil, Options{})
	mgr.SetAssignees(devM, devF)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	if !mgr.Post(Task{ID: "p1", Title: "プロジェクト", From: "owner"}) {
		t.Fatal("Post が false を返しました")
	}

	// サブタスクが 2 件作成されるまで待つ。
	if !waitFor(t, 2*time.Second, func() bool { return coord.count() == 2 }) {
		t.Fatalf("CreateSubtask 呼び出し回数 = %d, want 2", coord.count())
	}
	if titles := coord.createdTitles(); len(titles) != 2 || titles[0] != "A" || titles[1] != "B" {
		t.Errorf("作成したサブタスク = %v, want [A B]", titles)
	}

	// 各 dev の受信箱に担当の子タスクが届く。
	gotM := recvTask(t, devM.inbox)
	if gotM.Title != "A" || gotM.Assignee != "dev_m" {
		t.Errorf("dev_m の受信タスク = %+v, want title=A assignee=dev_m", gotM)
	}
	gotF := recvTask(t, devF.inbox)
	if gotF.Title != "B" || gotF.Assignee != "dev_f" {
		t.Errorf("dev_f の受信タスク = %+v, want title=B assignee=dev_f", gotF)
	}

	// 親タスクは working に更新される。
	if !updater.has("p1", "working") {
		t.Errorf("親タスク p1 が working になっていません: %v", updater.statuses("p1"))
	}
}

// TestManagerAggregatesProjectWhenAllSubtasksDone は全サブタスク完了で
// 親プロジェクトが done になり完了通知が出ることを検証する。
func TestManagerAggregatesProjectWhenAllSubtasksDone(t *testing.T) {
	notifier := newRecordingNotifier()
	updater := &fakeUpdater{}
	coord := newFakeCoordinator()
	coord.parentOf["c1"] = "p1"
	coord.statuses["p1"] = []TaskStatus{
		{ID: "sub-1", Status: "done"},
		{ID: "sub-2", Status: "done"},
	}

	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, llm.NewMock(), notifier, Options{})
	mgr.SetTaskUpdater(updater)
	mgr.SetTaskCoordinator(coord)

	mgr.Review(context.Background(), Task{ID: "c1", Assignee: "dev_m"}, Result{Status: "done", Summary: "ok"})

	if !updater.has("p1", "done") {
		t.Errorf("親タスク p1 が done になっていません: %v", updater.statuses("p1"))
	}
	if updater.has("p1", "failed") {
		t.Errorf("親タスク p1 が failed になっています: %v", updater.statuses("p1"))
	}
	if !noticeContains(notifier, "完了") {
		t.Errorf("完了通知がありません: %+v", notifier.noticesSnapshot())
	}
}

// TestManagerProjectFailedWhenSubtaskFails は 1 つでもサブタスクが失敗すると
// 親プロジェクトが failed になり要対応通知が出ることを検証する。
func TestManagerProjectFailedWhenSubtaskFails(t *testing.T) {
	notifier := newRecordingNotifier()
	updater := &fakeUpdater{}
	coord := newFakeCoordinator()
	coord.parentOf["c1"] = "p1"
	coord.statuses["p1"] = []TaskStatus{
		{ID: "sub-1", Status: "done"},
		{ID: "sub-2", Status: "failed"},
	}

	mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, llm.NewMock(), notifier, Options{})
	mgr.SetTaskUpdater(updater)
	mgr.SetTaskCoordinator(coord)

	mgr.Review(context.Background(), Task{ID: "c1", Assignee: "dev_m"}, Result{Status: "failed", Summary: "失敗"})

	if !updater.has("p1", "failed") {
		t.Errorf("親タスク p1 が failed になっていません: %v", updater.statuses("p1"))
	}
	if !noticeContains(notifier, "要対応") {
		t.Errorf("要対応通知がありません: %+v", notifier.noticesSnapshot())
	}
}
