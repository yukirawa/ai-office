package api

// input_test.go は §15「TUI からの入力」の say / task メッセージを
// WS 接続（observer の owner）から検証する。

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// dialOwner は observer（owner）として WS 接続し、hello を送って welcome まで読む。
// TUI は employees に居ないが employee_id = "owner" で接続する（§15.1）。
func dialOwner(t *testing.T, ctx context.Context, ts *httptest.Server) *websocket.Conn {
	t.Helper()

	conn, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"hello","employee_id":"owner","device_id":"tui","version":"0.1.0"}`)); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	readUntil(t, ctx, conn, "welcome")
	return conn
}

// TestSayStoresMessageAndBroadcastsNotice は say が #会議室 に from_id=owner で
// 保存され、送信者へ notice が返ることを検証する（§15.1）。
func TestSayStoresMessageAndBroadcastsNotice(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"おはよう"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	notice := readUntil(t, ctx, conn, "notice")
	if notice["text"] != "おはよう" {
		t.Errorf("notice.text = %v, want おはよう", notice["text"])
	}
	if notice["from"] != "owner" {
		t.Errorf("notice.from = %v, want owner", notice["from"])
	}

	waitFor(t, ctx, func() bool {
		msgs, err := srv.store.Messages("#会議室", 20)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "owner" && m.Content == "おはよう" {
				return true
			}
		}
		return false
	}, "say が #会議室 に from_id=owner で保存されません")
}

// TestSayDefaultChannel は channel 省略時に #会議室 へ寄せられることを検証する。
func TestSayDefaultChannel(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","text":"チャンネル省略"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool {
		msgs, err := srv.store.Messages("#会議室", 20)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "owner" && m.Content == "チャンネル省略" {
				return true
			}
		}
		return false
	}, "channel 省略の say が #会議室 に保存されません")
}

// TestSayEmptyTextReturnsError は空 text の say が invalid_say を返すことを検証する。
func TestSayEmptyTextReturnsError(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"   "}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	m := readUntil(t, ctx, conn, "error")
	if m["code"] != "invalid_say" {
		t.Errorf("error.code = %v, want invalid_say", m["code"])
	}

	// 空 text は保存しない。
	msgs, err := srv.store.Messages("#会議室", 20)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("空 text の say が保存されました: %+v", msgs)
	}
}

// TestTaskMessageCreatesTask は task が created_by=owner のタスクを作ることを検証する。
func TestTaskMessageCreatesTask(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"task","title":"資料を作る","description":"議事録",`+
			`"mode":"local","repo":"","base_branch":""}`)); err != nil {
		t.Fatalf("write task: %v", err)
	}

	waitFor(t, ctx, func() bool {
		tasks, err := srv.store.Tasks("", 10)
		if err != nil {
			return false
		}
		for _, task := range tasks {
			if task.CreatedBy == "owner" && task.Title == "資料を作る" {
				return true
			}
		}
		return false
	}, "task メッセージからタスクが作られません")
}

// TestTaskEmptyTitleReturnsError は title 空の task が invalid_task を返すことを検証する。
func TestTaskEmptyTitleReturnsError(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"task","title":"  ","description":"x"}`)); err != nil {
		t.Fatalf("write task: %v", err)
	}

	m := readUntil(t, ctx, conn, "error")
	if m["code"] != "invalid_task" {
		t.Errorf("error.code = %v, want invalid_task", m["code"])
	}

	tasks, err := srv.store.Tasks("", 10)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("空 title の task が保存されました: %+v", tasks)
	}
}

// TestSayTriggersChatReply は chat 役がいるとき say が chat の返信を招くことを検証する（§15.1）。
func TestSayTriggersChatReply(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chat := agents.NewChatAgent("chat", persona.Load(`{"name":"チャット"}`),
		llm.NewMock("モックの返事です"), srv, agents.Options{Channel: "#会議室"})
	srv.SetChat(chat)
	go chat.Run(ctx)

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"おはよう"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool {
		msgs, err := srv.store.Messages("#会議室", 20)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "chat" && m.Content == "モックの返事です" {
				return true
			}
		}
		return false
	}, "say に対する chat 役の返信が #会議室 に現れません")
}
