package api

// phase4_test.go は Phase 4（関係値・chat 役・TUI 用タスク）を api から検証する。

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
	"github.com/yukirawa/ai-office/server/internal/store"
)

func TestOfficeStateIncludesTasks(t *testing.T) {
	rig := newTaskTestRig(t, "")
	ctx := context.Background()

	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{Title: "表示テスト", From: "owner"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	snap := rig.srv.snapshot(ctx)
	found := false
	for _, task := range snap.Tasks {
		if task.ID == id {
			found = true
			if task.Title != "表示テスト" {
				t.Errorf("title = %q", task.Title)
			}
		}
	}
	if !found {
		t.Errorf("office_state.tasks に作ったタスクが含まれていません: %+v", snap.Tasks)
	}
}

func TestChatEndpointReplies(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	chat := agents.NewChatAgent("chat", persona.Load(`{"name":"チャット"}`),
		llm.NewMock("モックの返事です"), rig.srv, agents.Options{Channel: "#会議室"})
	rig.srv.SetChat(chat)
	go chat.Run(ctx)

	resp, err := http.Post(rig.ts.URL+"/api/chat", "application/json",
		strings.NewReader(`{"message":"おはよう"}`))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("status = %d, want 202 (body=%v)", resp.StatusCode, body)
	}

	waitFor(t, context.Background(), func() bool {
		msgs, err := rig.st.Messages("#会議室", 20)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "chat" && strings.Contains(m.Content, "モックの返事") {
				return true
			}
		}
		return false
	}, "chat 役の返信が #会議室 に現れません")
}

func TestTaskCompletionAdjustsRelationships(t *testing.T) {
	rig := newTaskTestRig(t, "")
	rig.srv.Manager().SetRelationships(persona.NewService(rig.st))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	assigned := make(chan string, 4)
	startFakeWorker(t, ctx, wsURL(rig.ts.URL), assigned)

	waitFor(t, ctx, func() bool {
		_, ok := rig.reg.Get("dev_m")
		return ok
	}, "dev_m が在席になりません")

	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{Title: "関係値テスト", From: "owner"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	select {
	case <-assigned:
	case <-ctx.Done():
		t.Fatal("task_assign が届きませんでした")
	}

	waitFor(t, ctx, func() bool {
		task, err := rig.st.Task(id)
		return err == nil && task.Status == "done"
	}, "タスクが done になりません")

	// 成功レビューで mgr -> dev_m の affinity が上がっているはず（初期は未作成=0）。
	rels, err := rig.st.Relationships("mgr")
	if err != nil {
		t.Fatalf("Relationships: %v", err)
	}
	aff := -999
	for _, r := range rels {
		if r.ToID == "dev_m" {
			aff = r.Affinity
		}
	}
	if aff < 2 {
		t.Errorf("mgr -> dev_m の affinity = %d, want >= 2", aff)
	}
}

// TestRelationshipsPersistedAfterRestart は関係値が store に永続化されることを確認する。
func TestRelationshipsPersistedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rel.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	svc := persona.NewService(st)
	if err := svc.Adjust(context.Background(), "mgr", "dev_f", 4, 2); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	rels, err := st2.Relationships("mgr")
	if err != nil {
		t.Fatalf("Relationships: %v", err)
	}
	for _, r := range rels {
		if r.ToID == "dev_f" {
			if r.Affinity != 4 || r.Trust != 2 {
				t.Errorf("永続化された関係値が不一致: %+v", r)
			}
			return
		}
	}
	t.Fatal("dev_f への関係値が永続化されていません")
}
