package agents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// relReviewRecorder は関係値更新とタスク状態更新を 1 つの順序列に記録する。
// 「関係値 → タスク状態」の順で呼ばれること（および deltas）を検証するために使う。
type relReviewRecorder struct {
	mu     sync.Mutex
	events []string
	relErr error
}

func (r *relReviewRecorder) Adjust(_ context.Context, fromID, toID string, affinityDelta, trustDelta int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("rel:%s->%s:%d/%d", fromID, toID, affinityDelta, trustDelta))
	return r.relErr
}

func (r *relReviewRecorder) UpdateTask(_ context.Context, taskID, status, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("task:%s:%s", taskID, status))
	return nil
}

func (r *relReviewRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Review 成功時は mgr->assignee と assignee->mgr の両方を更新し、その後タスクを done にする。
func TestReviewAdjustsRelationshipsOnSuccess(t *testing.T) {
	rec := &relReviewRecorder{}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	mgr.SetTaskUpdater(rec)
	mgr.SetRelationships(rec)

	mgr.Review(context.Background(), Task{ID: "t1", Title: "x", Assignee: "dev_m"},
		Result{Status: "done", Summary: "ok"})

	want := []string{
		"rel:mgr->dev_m:2/1",
		"rel:dev_m->mgr:1/0",
		"task:t1:done",
	}
	if got := rec.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// Review 失敗時は mgr->assignee のみを強めに下げ、タスクを failed にする。
func TestReviewAdjustsRelationshipsOnFailure(t *testing.T) {
	rec := &relReviewRecorder{}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	mgr.SetTaskUpdater(rec)
	mgr.SetRelationships(rec)

	mgr.Review(context.Background(), Task{ID: "t2", Assignee: "dev_m"},
		Result{Status: "failed", Summary: "ng"})

	want := []string{
		"rel:mgr->dev_m:-3/-2",
		"task:t2:failed",
	}
	if got := rec.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// Assignee 未設定なら関係値は触らない（タスク状態更新のみ）。
func TestReviewSkipsRelationshipsWithoutAssignee(t *testing.T) {
	rec := &relReviewRecorder{}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	mgr.SetTaskUpdater(rec)
	mgr.SetRelationships(rec)

	mgr.Review(context.Background(), Task{ID: "t3"}, Result{Status: "done", Summary: "ok"})

	want := []string{"task:t3:done"}
	if got := rec.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// RelationshipUpdater がエラーを返しても Review は中断せずタスク状態を更新する。
func TestReviewRelationshipErrorDoesNotAbort(t *testing.T) {
	rec := &relReviewRecorder{relErr: errors.New("関係値ストア障害")}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	mgr.SetTaskUpdater(rec)
	mgr.SetRelationships(rec)

	mgr.Review(context.Background(), Task{ID: "t4", Assignee: "dev_m"},
		Result{Status: "done", Summary: "ok"})

	want := []string{
		"rel:mgr->dev_m:2/1",
		"rel:dev_m->mgr:1/0",
		"task:t4:done",
	}
	if got := rec.snapshot(); !equalStrings(got, want) {
		t.Fatalf("エラー時も Review は続行すべき: events = %v, want %v", got, want)
	}
}

// SetRelationships 未設定なら関係値は触らない。
func TestReviewWithoutRelationshipUpdater(t *testing.T) {
	rec := &relReviewRecorder{}
	mgr := NewManager("mgr", persona.Persona{}, llm.NewMock(), nil, Options{})
	mgr.SetTaskUpdater(rec)

	mgr.Review(context.Background(), Task{ID: "t5", Assignee: "dev_m"},
		Result{Status: "done", Summary: "ok"})

	want := []string{"task:t5:done"}
	if got := rec.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
