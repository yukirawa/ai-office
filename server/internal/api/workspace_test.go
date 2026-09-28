package api

// workspace_test.go は作業先パスの抽出/許可判定（tasks.go）と、同一社員の
// 二重接続を新しい接続で置き換える挙動（ws.go の evictEmployee）の回帰テスト。
//
// 既存ヘルパ（newTestServer / newTaskTestRig / wsURL / readUntil など）を再利用する。

import (
	"context"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/presence"
)

// TestExtractWorkspacePath は文字列中の「絶対パスらしきトークン」の抽出を検証する。
// URL の "://" や相対パス（1/2）は誤検出しない。
func TestExtractWorkspacePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"日本語が隣接", "/home/u/Dev/siteに、天気", "/home/u/Dev/site"},
		{"URLは誤検出しない", "https://open-meteo.comから拾う", ""},
		{"数値の分数は誤検出しない", "1/2 を計算する", ""},
		{"文中の絶対パス", "ログイン画面を /home/u/Dev/app に作る", "/home/u/Dev/app"},
		{"パス単体", "/etc/passwd", "/etc/passwd"},
		{"パス無し", "パス無しのタスク", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractWorkspacePath(tc.in); got != tc.want {
				t.Errorf("extractWorkspacePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestWorkspaceAllowed は AllowedRoots との前方一致判定を検証する。
// 許可ルート未設定なら常に true。
func TestWorkspaceAllowed(t *testing.T) {
	rig := newTaskTestRig(t, "")
	rig.cfg.AllowedRoots = []string{"/home/u/Dev"}

	allowed := []string{"/home/u/Dev/a", "/home/u/Dev"}
	for _, p := range allowed {
		if !rig.srv.workspaceAllowed(p) {
			t.Errorf("workspaceAllowed(%q) = false, want true", p)
		}
	}

	// 前方一致の境界（"/home/u/Dever" は "/home/u/Dev" 配下ではない）と範囲外。
	disallowed := []string{"/home/u/Dever", "/etc/passwd", "/home/u/Devil"}
	for _, p := range disallowed {
		if rig.srv.workspaceAllowed(p) {
			t.Errorf("workspaceAllowed(%q) = true, want false", p)
		}
	}

	// AllowedRoots が空なら任意のパスを許可する。
	rig.cfg.AllowedRoots = nil
	for _, p := range []string{"/etc/passwd", "/home/u/Dev/a", "/"} {
		if !rig.srv.workspaceAllowed(p) {
			t.Errorf("AllowedRoots 空: workspaceAllowed(%q) = false, want true", p)
		}
	}
}

// TestCreateTaskRejectsDisallowedWorkspace は CreateTask がタイトル中の絶対パスを
// 自動抽出し、許可ルート外ならエラー、許可ルート内ならタスクを作成することを検証する。
func TestCreateTaskRejectsDisallowedWorkspace(t *testing.T) {
	rig := newTaskTestRig(t, "")
	rig.cfg.AllowedRoots = []string{"/home/u/Dev"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := rig.srv.CreateTask(ctx, CreateTaskInput{Title: "/etc/に書いて", From: "owner"})
	if err == nil {
		t.Fatal("許可外の作業先なのにエラーになりませんでした")
	}
	if !strings.Contains(err.Error(), "許可されていません") {
		t.Errorf("エラーメッセージが想定外: %v", err)
	}

	// 許可ルート内ならエラーにならず、task_id が返る（mgr の有無は問わない）。
	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{
		Title: "ログイン画面を /home/u/Dev/app に作る",
		From:  "owner",
	})
	if err != nil {
		t.Fatalf("許可ルート内の CreateTask が失敗しました: %v", err)
	}
	if id == "" {
		t.Error("task_id が空です")
	}
}

// waitForPeerClose はサーバー側から接続が閉じられる（close frame を受けて
// conn.Read がエラーを返す）まで待ち、そのエラーを返す。readUntil と違い
// read エラーを Fatal せず、閉じられるまでメッセージを読み進めて待つ。
//
// nhooyr の Read は context のキャンセルで接続自身を close してしまうため、
// ポーリングのために 1 回ごとに読み取りを打ち切ったりはしない。代わりに
// 短いタイムアウト付きの 1 つの context で待ち切る（固定 sleep も使わない）。
func waitForPeerClose(t *testing.T, ctx context.Context, conn *websocket.Conn) error {
	t.Helper()

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(rctx)
		if err == nil {
			// welcome / office_state / notice などを読み飛ばして待ち続ける。
			continue
		}
		if rctx.Err() != nil {
			t.Fatalf("サーバーからの切断を検出できませんでした: %v", err)
		}
		return err
	}
}

// TestDuplicateEmployeeConnectionIsReplaced は同一社員の接続が新しい接続で
// 置き換えられる（旧接続が閉じられ、在席は維持される）ことを検証する。
func TestDuplicateEmployeeConnectionIsReplaced(t *testing.T) {
	_, reg, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	hello := []byte(`{"type":"hello","employee_id":"dev_m","device_id":"zenbook","version":"0.1.0"}`)

	// 接続 A（1 本目）: dev_m として出勤。
	connA, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial A: %v", err)
	}
	defer connA.CloseNow()
	if err := connA.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello A: %v", err)
	}
	readUntil(t, ctx, connA, "welcome")

	// 接続 B（2 本目）: 同じ dev_m で接続すると、サーバーが A を置き換える。
	connB, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial B: %v", err)
	}
	defer connB.CloseNow()
	if err := connB.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello B: %v", err)
	}
	readUntil(t, ctx, connB, "welcome")

	// A はサーバーから閉じられる（close frame 受信で Read がエラー）。
	err = waitForPeerClose(t, ctx, connA)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("閉じコード = %d, want %d (err=%v)", got, websocket.StatusPolicyViolation, err)
	}

	// 置き換えであって退勤ではないため、在席は維持される。
	if e, ok := reg.Get("dev_m"); !ok || e.Status != presence.StatusOnline {
		t.Errorf("置き換えで dev_m が退勤扱いになっています: %+v ok=%v", e, ok)
	}
}
