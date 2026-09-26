package agents

// converse_test.go は Converser / Modeler（Agent）の共通挙動を検証する。
//
// - mgr / dev / chat の Converse が 1 件の通知を投稿する
// - SetModel で実行時にモデルを差し替えられ、SetModel("") で既定へ戻る
// - タスク経路（mgr の計画 / dev の計画）も現在のモデルを読む
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

// SetModel は実行時のモデルを差し替え、空文字で設定既定へ戻す。
// Converse がその都度 Model() を読むことを検証する。
func TestSetModelSwapsAndResets(t *testing.T) {
	models := []struct {
		name        string
		opts        Options
		wantDefault string
	}{
		{name: "configured", opts: Options{Model: "base-model"}, wantDefault: "base-model"},
		{name: "fallback_default", opts: Options{}, wantDefault: DefaultModel},
	}
	for _, tc := range agentCases() {
		for _, mc := range models {
			t.Run(tc.name+"/"+mc.name, func(t *testing.T) {
				notifier := newRecordingNotifier()
				client := llm.NewMock("はい", "いいえ")
				a := tc.factory(notifier, client, mc.opts)

				if got := a.Model(); got != mc.wantDefault {
					t.Errorf("初期 Model() = %q, want %q", got, mc.wantDefault)
				}

				a.SetModel("premium-x")
				if got := a.Model(); got != "premium-x" {
					t.Errorf("SetModel(\"premium-x\") 後 Model() = %q, want premium-x", got)
				}
				a.Converse(context.Background(), "#会議室", "ひとつめ")

				a.SetModel("")
				if got := a.Model(); got != mc.wantDefault {
					t.Errorf("SetModel(\"\") 後 Model() = %q, want %q", got, mc.wantDefault)
				}
				a.Converse(context.Background(), "#会議室", "ふたつめ")

				reqs := client.Requests()
				if len(reqs) != 2 {
					t.Fatalf("LLM 呼び出し回数 = %d, want 2", len(reqs))
				}
				if got := reqs[0].Model; got != "premium-x" {
					t.Errorf("1 回目の Model = %q, want premium-x", got)
				}
				if got := reqs[1].Model; got != mc.wantDefault {
					t.Errorf("2 回目の Model = %q, want %q", got, mc.wantDefault)
				}
			})
		}
	}
}

// タスク経路（mgr の計画 / dev の計画）も現在のモデルを読むことを検証する。
func TestSetModelAffectsTaskRequest(t *testing.T) {
	t.Run("mgr", func(t *testing.T) {
		notifier := newRecordingNotifier()
		client := llm.NewMock("計画A", "計画B")
		mgr := NewManager("mgr", persona.Persona{Name: "mgr"}, client, notifier, Options{})
		mgr.SetModel("premium-mgr")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go mgr.Run(ctx)

		if !mgr.Post(Task{ID: "t1", Title: "x", From: "owner"}) {
			t.Fatal("Post が false")
		}
		if !waitFor(t, 2*time.Second, func() bool { return len(client.Requests()) >= 1 }) {
			t.Fatal("1 件目の LLM 呼び出しがありません")
		}

		mgr.SetModel("")
		if !mgr.Post(Task{ID: "t2", Title: "y", From: "owner"}) {
			t.Fatal("Post が false")
		}
		if !waitFor(t, 2*time.Second, func() bool { return len(client.Requests()) >= 2 }) {
			t.Fatal("2 件目の LLM 呼び出しがありません")
		}

		reqs := client.Requests()
		if got := reqs[0].Model; got != "premium-mgr" {
			t.Errorf("1 件目の Model = %q, want premium-mgr", got)
		}
		if got := reqs[1].Model; got != DefaultModel {
			t.Errorf("2 件目の Model = %q, want %q", got, DefaultModel)
		}
	})

	t.Run("dev", func(t *testing.T) {
		notifier := newRecordingNotifier()
		client := llm.NewMock() // 既定の散文 → フォールバック、1 タスク 1 呼び出し
		dispatcher := &fakeDispatcher{awaitResult: Result{TaskID: "t", Status: "done", Summary: "ok"}}
		reviewer := &fakeReviewer{}
		dev := NewDevAgent("dev", persona.Persona{Name: "dev"}, client, notifier,
			dispatcher, nil, nil, reviewer, Options{})
		dev.SetModel("premium-dev")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go dev.Run(ctx)

		if !dev.Post(Task{ID: "t1", Title: "x", Plan: "p"}) {
			t.Fatal("Post が false")
		}
		if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 1 }) {
			t.Fatal("1 件目のレビューがありません")
		}

		dev.SetModel("")
		if !dev.Post(Task{ID: "t2", Title: "y", Plan: "p"}) {
			t.Fatal("Post が false")
		}
		if !waitFor(t, 2*time.Second, func() bool { return reviewer.count() == 2 }) {
			t.Fatal("2 件目のレビューがありません")
		}

		reqs := client.Requests()
		if len(reqs) < 2 {
			t.Fatalf("LLM 呼び出し回数 = %d, want >= 2", len(reqs))
		}
		if got := reqs[0].Model; got != "premium-dev" {
			t.Errorf("1 件目の Model = %q, want premium-dev", got)
		}
		if got := reqs[1].Model; got != DefaultModel {
			t.Errorf("2 件目の Model = %q, want %q", got, DefaultModel)
		}
	})
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
