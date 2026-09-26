package api

// ws_test.go は WebSocket の結合テスト（設計書 §10 の「WS接続 → check-in →
// heartbeat → check-out」）を、httptest サーバ + nhooyr の Dial で検証する。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/economy"
	"github.com/yukirawa/ai-office/server/internal/presence"
	"github.com/yukirawa/ai-office/server/internal/store"
)

func newTestServer(t *testing.T) (*Server, *presence.Registry, *httptest.Server) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		t.Fatalf("SeedEmployees: %v", err)
	}

	cfg := &config.Config{HeartbeatTimeout: 90 * time.Second}
	reg := presence.NewRegistry()
	econ := economy.New(st)

	srv := New(Deps{Cfg: cfg, Store: st, Presence: reg, Economy: econ})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, reg, ts
}

// wsURL は httptest の URL を ws:// に変換する。
func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
}

// readUntil は指定 type のメッセージが来るまで読み飛ばして返す。
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read (%s 待ち): %v", want, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m["type"] == want {
			return m
		}
	}
	t.Fatalf("%s が来ませんでした", want)
	return nil
}

func TestWebSocketHandshakeAndCheckInOut(t *testing.T) {
	_, reg, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseNow()

	// hello を送る。
	hello := `{"type":"hello","employee_id":"dev_m","device_id":"zenbook","version":"0.1.0"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(hello)); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// welcome が返り、office.online に自分が入っていること。
	welcome := readUntil(t, ctx, conn, "welcome")
	if welcome["session_id"] == "" || welcome["session_id"] == nil {
		t.Errorf("welcome.session_id が空です: %v", welcome)
	}
	office, ok := welcome["office"].(map[string]any)
	if !ok {
		t.Fatalf("welcome.office の形が不正です: %v", welcome["office"])
	}
	online := toStrings(office["online"])
	if !contains(online, "dev_m") {
		t.Errorf("office.online に dev_m がいません: %v", online)
	}

	// office_state が届くこと。
	state := readUntil(t, ctx, conn, "office_state")
	if !contains(toStrings(state["online"]), "dev_m") {
		t.Errorf("office_state.online に dev_m がいません: %v", state["online"])
	}

	// Registry 上でも在席になっていること。
	if e, ok := reg.Get("dev_m"); !ok || e.Status != presence.StatusOnline {
		t.Errorf("presence が online ではありません: %+v ok=%v", e, ok)
	}

	// heartbeat を送る。
	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"heartbeat","ts":"2026-09-26T12:34:56Z"}`)); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	// bye を送る。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"bye","reason":"shutdown"}`)); err != nil {
		t.Fatalf("write bye: %v", err)
	}

	// 退勤が Registry に反映されるまで待つ。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := reg.Get("dev_m"); ok && e.Status == presence.StatusOffline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("bye 後も dev_m が online のままです")
}

func TestWebSocketRejectsNonHelloFirst(t *testing.T) {
	_, _, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseNow()

	// いきなり heartbeat を送ると error が返るはず（§4.2 固定）。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"heartbeat","ts":"x"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := readUntil(t, ctx, conn, "error")
	if m["code"] != "invalid_hello" {
		t.Errorf("error.code = %v, want invalid_hello", m["code"])
	}
}

func TestUnknownEmployeeIsObserver(t *testing.T) {
	_, reg, ts := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(ts.URL), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"hello","employee_id":"tui","device_id":"tui","version":"0.1.0"}`)); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	readUntil(t, ctx, conn, "welcome")

	// 観測者は在席に数えない。
	if online := reg.Online(); contains(online, "tui") {
		t.Errorf("observer が online に含まれています: %v", online)
	}
}

func TestHealthzAndEmployees(t *testing.T) {
	_, _, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/api/employees")
	if err != nil {
		t.Fatalf("GET /api/employees: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("employees status = %d", resp2.StatusCode)
	}
	var emps []map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&emps); err != nil {
		t.Fatalf("decode employees: %v", err)
	}
	if len(emps) != 4 {
		t.Errorf("社員数 = %d, want 4", len(emps))
	}
	for _, e := range emps {
		if e["status"] != string(presence.StatusOffline) {
			t.Errorf("初期状態は offline のはず: %v", e)
		}
	}
}

func toStrings(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
