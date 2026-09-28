package agents

// converse_test.go は Converser（Agent）の共通挙動を検証する。
//
// - mgr / dev / chat の Converse が 1 件の通知を投稿する
// - DevAgent.Run が起動時の【出勤】通知を出さない（重複解消の回帰防止）

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

// agentCase は mgr / dev / chat を共通のテストで扱うためのファクトリ。
type agentCase struct {
	name    string // テスト名
	display string // ペルソナの表示名
	id      string // 社員 ID（通知の fromID）
	factory func(notifier Notifier, client llm.Client, opts Options) Agent
}

func agentCases() []agentCase {
	return []agentCase{
		{
			name:    "mgr",
			display: "ミカ",
			id:      "mgr",
			factory: func(n Notifier, c llm.Client, o Options) Agent {
				return NewManager("mgr", persona.Persona{Name: "ミカ", Role: "管理職"}, c, n, o)
			},
		},
		{
			name:    "dev",
			display: "ソラ",
			id:      "dev",
			factory: func(n Notifier, c llm.Client, o Options) Agent {
				return NewDevAgent("dev", persona.Persona{Name: "ソラ", Role: "開発"}, c, n, nil, nil, nil, nil, o)
			},
		},
		{
			name:    "chat",
			display: "チャット",
			id:      "chat",
			factory: func(n Notifier, c llm.Client, o Options) Agent {
				return NewChatAgent("chat", persona.Persona{Name: "チャット", Role: "雑談"}, c, n, o)
			},
		},
	}
}

// mgr / dev / chat の Converse は、チャンネルへ自分の ID で 1 件だけ投稿する。
func TestConversePostsNotice(t *testing.T) {
	for _, tc := range agentCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			client := llm.NewMock("はい、承知しました。")
			a := tc.factory(notifier, client, Options{Channel: "#会議室"})

			a.Converse(context.Background(), "#会議室", "おはよう")

			notices := notifier.noticesSnapshot()
			if len(notices) != 1 {
				t.Fatalf("通知数 = %d, want 1: %+v", len(notices), notices)
			}
			n := notices[0]
			if n.fromID != tc.id {
				t.Errorf("fromID = %q, want %q", n.fromID, tc.id)
			}
			if n.channel != "#会議室" {
				t.Errorf("channel = %q, want #会議室", n.channel)
			}
			if strings.TrimSpace(n.text) == "" {
				t.Error("通知テキストが空です")
			}
			if n.text != "はい、承知しました。" {
				t.Errorf("投稿テキスト = %q, want 応答そのもの", n.text)
			}

			// LLM には会話用のシステムプロンプトと 1 往復のユーザーメッセージが渡る。
			reqs := client.Requests()
			if len(reqs) != 1 {
				t.Fatalf("LLM 呼び出し回数 = %d, want 1", len(reqs))
			}
			if !strings.Contains(reqs[0].System, "1〜2 文") {
				t.Errorf("システムプロンプトに会話指示が含まれていません: %q", reqs[0].System)
			}
			if want := fmt.Sprintf("あなたは「%s」です", tc.display); !strings.Contains(reqs[0].System, want) {
				t.Errorf("システムプロンプトに名前が含まれていません: %q", reqs[0].System)
			}
			if len(reqs[0].Messages) == 0 || !strings.Contains(reqs[0].Messages[0].Content, "おはよう") {
				t.Errorf("プロンプトがユーザーメッセージに含まれていません: %+v", reqs[0].Messages)
			}
		})
	}
}

// channel が空なら Options.Channel（既定）へ投稿する。
func TestConverseEmptyChannelUsesDefault(t *testing.T) {
	for _, tc := range agentCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			a := tc.factory(notifier, llm.NewMock("はい"), Options{Channel: "#雑談"})

			a.Converse(context.Background(), "", "やあ")

			notices := notifier.noticesSnapshot()
			if len(notices) != 1 {
				t.Fatalf("通知数 = %d, want 1: %+v", len(notices), notices)
			}
			if notices[0].channel != "#雑談" {
				t.Errorf("channel = %q, want #雑談（既定）", notices[0].channel)
			}
		})
	}
}

// LLM エラー時は panic せず、投稿もしない。
func TestConverseSurvivesLLMError(t *testing.T) {
	for _, tc := range agentCases() {
		t.Run(tc.name, func(t *testing.T) {
			notifier := newRecordingNotifier()
			a := tc.factory(notifier, &flakyClient{}, Options{Channel: "#会議室"})

			a.Converse(context.Background(), "#会議室", "やあ")

			if got := notifier.noticesSnapshot(); len(got) != 0 {
				t.Errorf("エラー時に通知が投稿されました: %+v", got)
			}
		})
	}
}

// DevAgent.Run は起動時の【出勤】通知を出さない（worker 側が挨拶するため重複解消）。
// 起動の状態公開（idle）は残っていることを合わせて確認する。
func TestDevAgentRunHasNoStartupCheckinNotice(t *testing.T) {
	notifier := newRecordingNotifier()
	dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, llm.NewMock(), notifier,
		nil, nil, nil, nil, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)

	// 起動の setState(idle) が公開されるまで待つ。
	if !waitFor(t, time.Second, func() bool { return len(notifier.statesFor("dev")) >= 1 }) {
		t.Fatal("dev の起動状態が公開されませんでした")
	}
	// 旧実装はこの直後に【出勤】を投稿していた。余裕を持って確認する。
	time.Sleep(30 * time.Millisecond)

	for _, n := range notifier.noticesSnapshot() {
		if strings.Contains(n.text, "出勤") {
			t.Errorf("dev が起動時の出勤通知を投稿しました: %q", n.text)
		}
	}
	if got := notifier.statesFor("dev"); !containsSubsequence(got, []string{StateIdle}) {
		t.Errorf("dev の起動状態 = %v, want idle を含む", got)
	}
}
