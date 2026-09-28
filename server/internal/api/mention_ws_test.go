package api

// mention_ws_test.go は #会議室 の @宛先（@id / @all / 本文なし）が
// WS の say 経由で正しいエージェントの Converse へ振り分けられることを検証する（§15.1）。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// converseCall は Converse 1 回分の引数。
type converseCall struct {
	channel string
	prompt  string
}

// recordingAgent は Converse の呼び出しを記録する agents.Agent のテスト用実装。
// 引数まで残すために独自型にしている。
type recordingAgent struct {
	mu    sync.Mutex
	calls []converseCall
}

// Compile-time check: recordingAgent は agents.Agent を満たす。
var _ agents.Agent = (*recordingAgent)(nil)

func (r *recordingAgent) Converse(_ context.Context, channel, prompt string) {
	r.mu.Lock()
	r.calls = append(r.calls, converseCall{channel: channel, prompt: prompt})
	r.mu.Unlock()
}

// recorded は記録済みの Converse 呼び出しをコピーして返す。
func (r *recordingAgent) recorded() []converseCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]converseCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestMentionRoutesOnlyAddressedAgent は @mgr が mgr だけに振り分けられ、
// チャンネルとプロンプトが正しく渡ることを検証する。
func TestMentionRoutesOnlyAddressedAgent(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &recordingAgent{}
	devM := &recordingAgent{}
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr, "dev_m": devM})

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"@mgr 点呼です"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	// converseAsync は goroutine なのでポーリングして待つ。
	waitFor(t, ctx, func() bool { return len(mgr.recorded()) == 1 },
		"@mgr が mgr の Converse を呼びません")

	calls := mgr.recorded()
	if calls[0].channel != "#会議室" {
		t.Errorf("channel = %q, want #会議室", calls[0].channel)
	}
	if calls[0].prompt != "点呼です" {
		t.Errorf("prompt = %q, want 点呼です", calls[0].prompt)
	}

	// 宛先でない dev_m は呼ばれない（同期 dispatch なので記録が遅れる余地は無い）。
	if got := devM.recorded(); len(got) != 0 {
		t.Errorf("宛先でない dev_m が呼ばれました: %+v", got)
	}
}

// TestMentionAllConverseEveryAgent は @all が登録済みの全エージェントへ
// 同じ本文で振り分けられることを検証する。
func TestMentionAllConverseEveryAgent(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recorders := map[string]*recordingAgent{
		"mgr":   {},
		"dev_m": {},
		"dev_f": {},
		"chat":  {},
	}
	agentsMap := make(map[string]agents.Agent, len(recorders))
	for id, ag := range recorders {
		agentsMap[id] = ag
	}
	srv.SetAgents(agentsMap)

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"@all 点呼です"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool {
		for _, ag := range recorders {
			if len(ag.recorded()) == 0 {
				return false
			}
		}
		return true
	}, "@all が全エージェントの Converse を呼びません")

	for id, ag := range recorders {
		calls := ag.recorded()
		if calls[0].channel != "#会議室" || calls[0].prompt != "点呼です" {
			t.Errorf("%s への振り分けが不正: %+v", id, calls[0])
		}
	}
}

// TestMentionWithoutBodyUsesDefaultPrompt は "@mgr" のみ（本文なし）で
// 空でない既定プロンプトが渡ることを検証する。
func TestMentionWithoutBodyUsesDefaultPrompt(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &recordingAgent{}
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr})

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"@mgr"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool { return len(mgr.recorded()) == 1 },
		"本文なし @mgr が mgr の Converse を呼びません")

	calls := mgr.recorded()
	if calls[0].channel != "#会議室" {
		t.Errorf("channel = %q, want #会議室", calls[0].channel)
	}
	if calls[0].prompt == "" {
		t.Fatal("本文なし @mgr の prompt が空です")
	}
	if want := "呼びかけに一言返してください。"; calls[0].prompt != want {
		t.Errorf("prompt = %q, want %q", calls[0].prompt, want)
	}
}

// TestMentionUppercaseRoutesAddressedAgent は大文字の @MGR でも小文字の @mgr と
// 同じく mgr の Converse へ振り分けられることを検証する。
func TestMentionUppercaseRoutesAddressedAgent(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &recordingAgent{}
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr})

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"@MGR 点呼です"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool { return len(mgr.recorded()) == 1 },
		"大文字 @MGR が mgr の Converse を呼びません")

	calls := mgr.recorded()
	if calls[0].channel != "#会議室" || calls[0].prompt != "点呼です" {
		t.Errorf("@MGR への振り分けが不正: %+v", calls[0])
	}
}

// TestMentionUnknownTargetNotifiesMeetingRoom は未知の宛先 @nobody が
// どのエージェントも呼ばず、#会議室 に system の宛先不明通知を残すことを検証する。
func TestMentionUnknownTargetNotifiesMeetingRoom(t *testing.T) {
	srv, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &recordingAgent{}
	devM := &recordingAgent{}
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr, "dev_m": devM})

	conn := dialOwner(t, ctx, ts)

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"say","channel":"#会議室","text":"@nobody やあ"}`)); err != nil {
		t.Fatalf("write say: %v", err)
	}

	waitFor(t, ctx, func() bool {
		msgs, err := srv.store.Messages("#会議室", 20)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "system" && strings.Contains(m.Content, "宛先不明") && strings.Contains(m.Content, "@nobody") {
				return true
			}
		}
		return false
	}, "未知の宛先 @nobody の system 通知が #会議室 に現れません")

	// 登録エージェントは誰も会話を促されない。
	if got := mgr.recorded(); len(got) != 0 {
		t.Errorf("未知の宛先で mgr が呼ばれました: %+v", got)
	}
	if got := devM.recorded(); len(got) != 0 {
		t.Errorf("未知の宛先で dev_m が呼ばれました: %+v", got)
	}
}

// TestParseMentionWhitespaceAfterAt は "@ だけ" のように @ の直後が空白のとき、
// メンション無し扱いで本文全体が返ることを検証する。
func TestParseMentionWhitespaceAfterAt(t *testing.T) {
	target, body := parseMention("@ だけ")
	if target != "" || body != "@ だけ" {
		t.Errorf("parseMention(\"@ だけ\") = (%q, %q), want (\"\", \"@ だけ\")", target, body)
	}
}

// TestSayMentionlessTriggersChatReply はメンション無しの say が chat 役の
// 返信を招き、#会議室 に保存されることを検証する（TestSayTriggersChatReply と同様）。
func TestSayMentionlessTriggersChatReply(t *testing.T) {
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
	}, "メンション無しの say に対する chat 役の返信が #会議室 に現れません")
}
