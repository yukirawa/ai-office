package api

// initiative_test.go は RunInitiative（§16 自律）の回帰テスト。
//
// 登録エージェントの Initiative が返した本文を #会議室 に投稿し、本文先頭の
// @id / @all に応じて相手の Converse を 1 往復だけ振ることを検証する。
// 空文字を返した社員は投稿せず、自分自身への @id では Converse を呼ばない。
//
// 既存ヘルパ（newTestServer / waitFor / converseCall / recordingAgent）を再利用する。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/agents"
)

// initiativeFakeAgent は Initiative の戻り値と Converse 呼び出しを記録する
// agents.Agent + agents.InitiativeAgent のテスト用実装。
// 既存の recordingAgent / fakeAgent とは別名にして名前衝突を避ける。
type initiativeFakeAgent struct {
	text string // Initiative が返す本文（空文字なら投稿されない）。

	mu    sync.Mutex
	calls []converseCall // 記録には既存型 converseCall（mention_ws_test.go）を使う。
}

// Compile-time check: initiativeFakeAgent は agents.Agent と InitiativeAgent を満たす。
var (
	_ agents.Agent           = (*initiativeFakeAgent)(nil)
	_ agents.InitiativeAgent = (*initiativeFakeAgent)(nil)
)

// Converse は呼び出しを記録する（実際の LLM 呼び出しはしない）。
func (f *initiativeFakeAgent) Converse(_ context.Context, channel, prompt string) {
	f.mu.Lock()
	f.calls = append(f.calls, converseCall{channel: channel, prompt: prompt})
	f.mu.Unlock()
}

// Initiative は設定された本文を返す。
func (f *initiativeFakeAgent) Initiative(_ context.Context, _ string) string {
	return f.text
}

// recorded は記録済みの Converse 呼び出しをコピーして返す。
func (f *initiativeFakeAgent) recorded() []converseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]converseCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// assertNoConverse は一定時間 Converse が呼ばれないことを確認する（負の検証用）。
// converseAsync は goroutine で走るため、少しの間ポーリングして呼び出しの有無を見る。
func assertNoConverse(t *testing.T, ag *initiativeFakeAgent, msg string) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls := ag.recorded(); len(calls) != 0 {
			t.Fatalf("%s: %+v", msg, calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunInitiativePostsAndRoutesMention は mgr の自発発言が #会議室 に保存され、
// 本文先頭の @dev_f が dev_f の Converse へ振り分けられることを検証する。
func TestRunInitiativePostsAndRoutesMention(t *testing.T) {
	srv, _, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &initiativeFakeAgent{text: "@dev_f 進捗どう？"}
	devF := &initiativeFakeAgent{text: ""} // 空文字を返すので投稿しない。
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr, "dev_f": devF})

	srv.RunInitiative(ctx)

	// mgr の自発発言が from=mgr で #会議室 に保存されている。
	waitFor(t, ctx, func() bool {
		msgs, err := srv.store.Messages("#会議室", 10)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.FromID == "mgr" && m.Content == "@dev_f 進捗どう？" {
				return true
			}
		}
		return false
	}, "mgr の自発発言が #会議室 に保存されません")

	// @dev_f が dev_f の Converse を呼ぶ（converseAsync は goroutine なので待つ）。
	waitFor(t, ctx, func() bool { return len(devF.recorded()) == 1 },
		"@dev_f が dev_f の Converse を呼びません")

	calls := devF.recorded()
	if calls[0].channel != "#会議室" {
		t.Errorf("channel = %q, want #会議室", calls[0].channel)
	}
	if calls[0].prompt != "進捗どう？" {
		t.Errorf("prompt = %q, want 進捗どう？", calls[0].prompt)
	}
}

// TestRunInitiativeSkipsEmptyAndSelfMention は空文字を返した社員が投稿されず、
// 自分自身への @id（@mgr）では自分の Converse が呼ばれないことを検証する。
func TestRunInitiativeSkipsEmptyAndSelfMention(t *testing.T) {
	srv, _, _ := newTestServer(t)

	mgr := &initiativeFakeAgent{text: "@mgr 自分"}
	devF := &initiativeFakeAgent{text: ""} // 空文字を返す社員。
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr, "dev_f": devF})

	srv.RunInitiative(context.Background())

	msgs, err := srv.store.Messages("#会議室", 20)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	sawMgr := false
	for _, m := range msgs {
		// 空文字を返した dev_f は投稿されない。
		if m.FromID == "dev_f" {
			t.Errorf("空文字を返した dev_f の投稿が保存されました: %+v", m)
		}
		if m.FromID == "mgr" && m.Content == "@mgr 自分" {
			sawMgr = true
		}
	}
	// 自分自身への @mgr でも投稿自体は行われる。
	if !sawMgr {
		t.Errorf("#会議室 に mgr の自発発言が保存されていません: %+v", msgs)
	}

	// 自分自身への @mgr では mgr の Converse は呼ばれない。
	assertNoConverse(t, mgr, "自分自身への @mgr で mgr の Converse が呼ばれました")
}

// TestRunInitiativeAllMention は @all が送信者以外の全社員の Converse を呼び、
// 送信者自身は呼ばれないことを検証する。
func TestRunInitiativeAllMention(t *testing.T) {
	srv, _, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr := &initiativeFakeAgent{text: "@all 集合"}
	devF := &initiativeFakeAgent{text: ""}
	srv.SetAgents(map[string]agents.Agent{"mgr": mgr, "dev_f": devF})

	srv.RunInitiative(ctx)

	// @all は送信者以外の全員に振られる。まず dev_f の Converse を待つ。
	waitFor(t, ctx, func() bool { return len(devF.recorded()) == 1 },
		"@all が dev_f の Converse を呼びません")

	calls := devF.recorded()
	if calls[0].channel != "#会議室" {
		t.Errorf("channel = %q, want #会議室", calls[0].channel)
	}
	if calls[0].prompt != "集合" {
		t.Errorf("prompt = %q, want 集合", calls[0].prompt)
	}

	// 送信者 mgr 自身は @all の対象外。
	assertNoConverse(t, mgr, "@all で送信者 mgr の Converse が呼ばれました")
}
