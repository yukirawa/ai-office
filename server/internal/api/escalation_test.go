package api

// escalation_test.go は §16 のエスカレーション（質問/回答）を api 側の境界から
// 検証する。既存ヘルパ（newTaskTestRig / dialOwner / readUntil / waitFor）を再利用する。

import (
	"context"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// TestAskOwnerBroadcastsQuestion は AskOwner が #会議室 に mgr 名義で質問を保存し、
// 接続中のオーナーへ question を配信することを検証する。
func TestAskOwnerBroadcastsQuestion(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, rig.ts)

	if err := rig.srv.AskOwner(ctx, agents.Question{
		ID:     "q1",
		FromID: "dev_m",
		TaskID: "t1",
		Text:   "どう進めますか",
	}); err != nil {
		t.Fatalf("AskOwner: %v", err)
	}

	q := readUntil(t, ctx, conn, "question")
	if q["id"] != "q1" {
		t.Errorf("question.id = %v, want q1", q["id"])
	}
	if q["from"] != "dev_m" {
		t.Errorf("question.from = %v, want dev_m", q["from"])
	}
	if q["text"] != "どう進めますか" {
		t.Errorf("question.text = %v, want どう進めますか", q["text"])
	}
	if q["task_id"] != "t1" {
		t.Errorf("question.task_id = %v, want t1", q["task_id"])
	}

	// 履歴として mgr 名義の【質問】が #会議室 に保存されていること。
	msgs, err := rig.st.Messages("#会議室", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.FromID == config.EmployeeManagerID &&
			strings.Contains(m.Content, "【質問】") &&
			strings.Contains(m.Content, "dev_m") {
			found = true
		}
	}
	if !found {
		t.Errorf("【質問】が mgr 名義で #会議室 に保存されていません: %+v", msgs)
	}
}

// TestAskOwnerWithoutClientsFailsFast は接続クライアントが居ないとき
// AskOwner がブロックせず即エラーになることを検証する。
func TestAskOwnerWithoutClientsFailsFast(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rig.srv.AskOwner(ctx, agents.Question{
		ID:     "q1",
		FromID: "dev_m",
		Text:   "誰もいません",
	})
	if err == nil {
		t.Fatal("クライアント不在なら AskOwner はエラーになるべきです")
	}
}

// TestAnswerRoutesToManager は WS の answer が Manager.Answer へ届き、
// Escalate の待機が回答で解決されることを検証する。
func TestAnswerRoutesToManager(t *testing.T) {
	rig := newTaskTestRig(t, "")

	mgr := agents.NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, rig.srv,
		agents.Options{AnswerTimeout: 5 * time.Second})
	rig.srv.SetManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// hasClients を true にするためオーナーとして接続する。
	conn := dialOwner(t, ctx, rig.ts)

	type answerResult struct {
		text string
		ok   bool
	}
	done := make(chan answerResult, 1)
	go func() {
		ans, ok := mgr.Escalate(ctx, agents.Question{
			ID:     "q1",
			FromID: "dev_m",
			Text:   "どうしますか",
		})
		done <- answerResult{text: ans, ok: ok}
	}()

	// Escalate -> AskOwner 経由の question が届くはず。
	q := readUntil(t, ctx, conn, "question")
	if q["id"] != "q1" {
		t.Errorf("question.id = %v, want q1", q["id"])
	}

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"answer","question_id":"q1","text":"こうしてください"}`)); err != nil {
		t.Fatalf("write answer: %v", err)
	}

	select {
	case got := <-done:
		if !got.ok || got.text != "こうしてください" {
			t.Errorf("Escalate = (%q, %v), want (こうしてください, true)", got.text, got.ok)
		}
	case <-ctx.Done():
		t.Fatal("Escalate の結果が返りませんでした")
	}

	// 回答が送信者（owner）名義で #会議室 に保存されていること。
	waitFor(t, ctx, func() bool {
		msgs, err := rig.st.Messages("#会議室", 10)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "owner" && m.Content == "【回答】こうしてください" {
				return true
			}
		}
		return false
	}, "【回答】が owner 名義で #会議室 に保存されません")
}

// TestAnswerEmptyTextReturnsError は空 text の answer が invalid_answer を返すことを検証する。
func TestAnswerEmptyTextReturnsError(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, rig.ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"answer","question_id":"q1","text":"  "}`)); err != nil {
		t.Fatalf("write answer: %v", err)
	}

	m := readUntil(t, ctx, conn, "error")
	if m["code"] != "invalid_answer" {
		t.Errorf("error.code = %v, want invalid_answer", m["code"])
	}
}
