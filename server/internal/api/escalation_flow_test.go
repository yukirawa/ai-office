package api

// escalation_flow_test.go は §16 のエスカレーションを
// 「dev → mgr → api → WS → mgr → dev」の一気通貫で検証する統合テスト。
//
// 既存の個別テスト（agents/escalation_test.go, api/escalation_test.go）が経路の一部ずつを
// 見るのに対し、ここでは「担当者の疑問 → mgr 経由でオーナーへ質問 → オーナーが WS で回答 →
// 回答を踏まえて dev が再計画 → その actions で dispatch」までを通しで確認する。
// 既存ヘルパ（newTaskTestRig / dialOwner / waitFor）を再利用し、question の受信だけは
// 接続直後の office_state が重なっても安定するよう readUntilType で待つ。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// flowDispatcher は agents.Dispatcher のテスト実装。
// Dispatch で受け取った TaskAssign を記録し、Await は即座に done を返す。
// 実 worker（WS）を介さずに「回答後に dev がどの actions で dispatch したか」を検証できる。
//
// startFakeWorker は task_id しか公開しないため actions の中身を確認できない。
// 本テストでは経路の要（質問→回答→再計画）を確実に検証するため dispatcher だけを fake に置換する。
type flowDispatcher struct {
	mu      sync.Mutex
	assigns []agents.TaskAssign
}

func (d *flowDispatcher) Dispatch(_ context.Context, _ string, assign agents.TaskAssign) error {
	d.mu.Lock()
	d.assigns = append(d.assigns, assign)
	d.mu.Unlock()
	return nil
}

func (d *flowDispatcher) Await(_ context.Context, taskID string) (agents.Result, error) {
	return agents.Result{TaskID: taskID, Status: "done", Summary: "テスト用 dispatcher が完了しました"}, nil
}

// last は最後に dispatch された TaskAssign を返す。
func (d *flowDispatcher) last() (agents.TaskAssign, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.assigns) == 0 {
		return agents.TaskAssign{}, false
	}
	return d.assigns[len(d.assigns)-1], true
}

// readUntilType は指定 type のメッセージが来るまで読み続けて返す。
// 既存の readUntil は固定回数（20 通）で打ち切るため、接続直後の office_state や
// 起動通知が重なると question が上限を超えることがある。ここでは ctx の期限まで待つ。
func readUntilType(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("%s 待ちで read エラー: %v", want, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m["type"] == want {
			return m
		}
	}
}

// TestEscalationFlowEndToEnd は dev が疑問を上げ、オーナーが WS で回答し、
// その回答を踏まえて dev が再計画して dispatch するまでを一気通貫で検証する。
func TestEscalationFlowEndToEnd(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 共有 LLM。呼び出し順は mgr の計画 → dev の初回計画（question）→ dev の再計画（actions）。
	client := llm.NewMock(
		"計画: 担当 dev_m で進めます",
		`{"question":"何色にしますか"}`,
		`{"actions":[{"op":"write","path":"reports/x.md","content":"red"}]}`,
	)

	opts := agents.Options{AnswerTimeout: 5 * time.Second, Channel: "#会議室"}

	// mgr は rig.srv を Notifier（= OwnerChannel 実装）として使い、質問をオーナーへ上げる。
	mgr := agents.NewManager("mgr", persona.Persona{Name: "ミカ"}, client, rig.srv, opts)

	// dev の dispatcher だけを fake に差し替える（updater / notifier は rig.srv）。
	dispatcher := &flowDispatcher{}
	dev := agents.NewDevAgent("dev_m", persona.Persona{Name: "タクミ"}, client, rig.srv,
		dispatcher, rig.srv, nil, mgr, opts)

	mgr.SetAssignees(dev)
	mgr.SetTaskUpdater(rig.srv)
	dev.SetEscalator(mgr)
	// rig 付属の mgr ではなく、テスト用 mgr をサーバーの窓口にする。
	rig.srv.SetManager(mgr)

	go mgr.Run(ctx)
	go dev.Run(ctx)

	// オーナー（TUI 相当）として WS 接続する。
	conn := dialOwner(t, ctx, rig.ts)

	// 計画が dev_m を名指しするので、mgr は dev_m に割り当てる。
	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{Title: "色を決めて", From: "owner"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// dev → mgr → api 経由の question がオーナーへ届く。
	q := readUntilType(t, ctx, conn, "question")
	if q["from"] != "dev_m" {
		t.Errorf("question.from = %v, want dev_m", q["from"])
	}
	if q["text"] != "何色にしますか" {
		t.Errorf("question.text = %v, want 何色にしますか", q["text"])
	}
	qid, _ := q["id"].(string)
	if qid == "" {
		t.Fatalf("question.id が空です: %v", q)
	}

	// オーナーが WS で回答する。api は AnswerQuestion で mgr へ渡す。
	if err := conn.Write(ctx, websocket.MessageText,
		fmt.Appendf(nil, `{"type":"answer","question_id":%q,"text":"赤でお願いします"}`, qid)); err != nil {
		t.Fatalf("write answer: %v", err)
	}

	// 回答を踏まえて dev が再計画し、タスクが done まで進む。
	waitFor(t, ctx, func() bool {
		task, err := rig.st.Task(id)
		return err == nil && task.Status == "done"
	}, "回答後にタスクが done になりません")

	// dev は回答を踏まえた actions（content=red）で dispatch しているはず。
	assign, ok := dispatcher.last()
	if !ok {
		t.Fatalf("dev が dispatch していません")
	}
	if len(assign.Actions) != 1 {
		t.Fatalf("Actions = %+v, want 1 件", assign.Actions)
	}
	if assign.Actions[0].Content != "red" {
		t.Errorf("Actions[0].Content = %q, want red（回答を踏まえた再計画）", assign.Actions[0].Content)
	}

	// #会議室 に【質問】（mgr 名義）と【回答】（owner 名義）が残っていること。
	waitFor(t, ctx, func() bool {
		msgs, err := rig.st.Messages("#会議室", 50)
		if err != nil {
			return false
		}
		var asked, answered bool
		for _, m := range msgs {
			if m.FromID == config.EmployeeManagerID && strings.Contains(m.Content, "【質問】") {
				asked = true
			}
			if m.FromID == "owner" && strings.Contains(m.Content, "【回答】赤でお願いします") {
				answered = true
			}
		}
		return asked && answered
	}, "#会議室 に【質問】/【回答】が保存されません")
}
