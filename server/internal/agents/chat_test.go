package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

func TestChatAgentRepliesAndStates(t *testing.T) {
	notifier := newRecordingNotifier()
	client := llm.NewMock("こんにちは、いい天気ですね！")
	chat := NewChatAgent("chat", persona.Persona{Name: "チャット", Role: "雑談係"}, client, notifier,
		Options{Channel: "#雑談"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)

	// 起動時の出勤通知。
	if !waitFor(t, time.Second, func() bool { return noticeContains(notifier, "【出勤】") }) {
		t.Fatal("出勤通知が投稿されませんでした")
	}

	if !chat.Post("今日は何してた？") {
		t.Fatal("Post が false を返しました")
	}

	// 投稿したプロンプトに応答が返る。
	if !waitFor(t, 2*time.Second, func() bool {
		return noticeContains(notifier, "こんにちは、いい天気ですね！")
	}) {
		t.Fatalf("返事が投稿されませんでした: %+v", notifier.noticesSnapshot())
	}

	// 状態遷移: idle(起動) → thinking → idle。
	if got := notifier.statesFor("chat"); !containsSubsequence(got, []string{StateIdle, StateThinking, StateIdle}) {
		t.Errorf("状態遷移 = %v, want subseq [idle thinking idle]", got)
	}
	if got := chat.State(); got != StateIdle {
		t.Errorf("State() = %q, want idle", got)
	}

	// 投稿先チャンネルと送信元。
	for _, n := range notifier.noticesSnapshot() {
		if n.fromID != "chat" {
			t.Errorf("fromID = %q, want chat", n.fromID)
		}
		if n.channel != "#雑談" {
			t.Errorf("channel = %q, want #雑談", n.channel)
		}
	}

	// LLM に渡したリクエストの検証。
	reqs := client.Requests()
	if len(reqs) != 1 {
		t.Fatalf("LLM 呼び出し回数 = %d, want 1", len(reqs))
	}
	if reqs[0].Model == "" {
		t.Error("Model が空です")
	}
	if !strings.Contains(reqs[0].System, "雑談") {
		t.Errorf("システムプロンプトに雑談指示が含まれていません: %q", reqs[0].System)
	}
	if len(reqs[0].Messages) == 0 || !strings.Contains(reqs[0].Messages[0].Content, "今日は何してた？") {
		t.Errorf("プロンプトがユーザーメッセージに含まれていません: %+v", reqs[0].Messages)
	}
}

// LLM がエラーを返しても Run は落ちず、次の入力に応答する。
func TestChatAgentSurvivesLLMError(t *testing.T) {
	notifier := newRecordingNotifier()
	client := &flakyClient{} // 1 回目はエラー、2 回目以降は "計画B"
	chat := NewChatAgent("chat", persona.Persona{Name: "チャット"}, client, notifier, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)

	chat.Post("ひとつめ")
	// エラー後に idle へ戻っている。
	if !waitFor(t, 2*time.Second, func() bool { return chat.State() == StateIdle }) {
		t.Fatal("エラー後に idle に戻りませんでした")
	}

	chat.Post("ふたつめ")
	if !waitFor(t, 2*time.Second, func() bool { return noticeContains(notifier, "計画B") }) {
		t.Fatalf("エラー後の応答がありません: %+v", notifier.noticesSnapshot())
	}
	if got := chat.State(); got != StateIdle {
		t.Errorf("State() = %q, want idle", got)
	}
}

func TestChatAgentPostFullAndClosedInbox(t *testing.T) {
	// Run していないので受信箱は消費されない。
	chat := NewChatAgent("chat", persona.Persona{}, llm.NewMock(), newRecordingNotifier(), Options{})
	for i := 0; i < inboxCapacity; i++ {
		if !chat.Post("p") {
			t.Fatalf("Post %d 回目が false を返しました", i+1)
		}
	}
	if chat.Post("overflow") {
		t.Fatal("満杯の受信箱への Post は false を返すべきです")
	}

	chat2 := NewChatAgent("chat", persona.Persona{}, llm.NewMock(), newRecordingNotifier(), Options{})
	close(chat2.Inbox())
	if chat2.Post("after-close") {
		t.Fatal("クローズ済みの受信箱への Post は false を返すべきです")
	}
}

func TestChatAgentNilClientDoesNotCrash(t *testing.T) {
	notifier := newRecordingNotifier()
	chat := NewChatAgent("chat", persona.Persona{Name: "チャット"}, nil, notifier, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go chat.Run(ctx)

	chat.Post("だれかいる？")
	if !waitFor(t, time.Second, func() bool { return chat.State() == StateIdle }) {
		t.Fatalf("nil client でも idle に戻るべきです: %q", chat.State())
	}
	// 出勤通知のみで、応答は投稿されない。
	for _, n := range notifier.noticesSnapshot() {
		if strings.Contains(n.text, "だれかいる") {
			t.Errorf("nil client で応答が投稿されました: %q", n.text)
		}
	}
}

func TestSanitizeChatReply(t *testing.T) {
	// 改行・連続空白を 1 つの空白に潰す。
	got := sanitizeChatReply("  おはよう\n\nございます  \r\n 元気？ ")
	if strings.Contains(got, "\n") {
		t.Errorf("改行が残っています: %q", got)
	}
	if got != "おはよう ございます 元気？" {
		t.Errorf("sanitize = %q, want 1 行化", got)
	}

	// 空は空のまま。
	if got := sanitizeChatReply("  \n  "); got != "" {
		t.Errorf("空白のみ = %q, want 空文字", got)
	}

	// 長すぎる応答は 500 rune に切り詰める（+ 末尾の …）。
	got = sanitizeChatReply(strings.Repeat("あ", 600))
	r := []rune(got)
	if len(r) > chatMaxReplyRunes+1 {
		t.Errorf("切り詰め後 = %d runes, want <= %d", len(r), chatMaxReplyRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("切り詰め後の末尾に … がありません: %q", got)
	}
}
