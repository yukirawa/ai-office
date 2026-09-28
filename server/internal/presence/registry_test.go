package presence

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// fixedClock はテスト用に時刻を固定・前進させるためのヘルパ。
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFixedClock(t time.Time) *fixedClock { return &fixedClock{t: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRegistry(t0 time.Time) (*Registry, *fixedClock) {
	c := newFixedClock(t0)
	r := NewRegistry()
	r.now = c.Now
	return r, c
}

func TestCheckInAndOnline(t *testing.T) {
	r, _ := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))

	// 順不同で出勤。Online は ID 昇順にソートされるはず。
	r.CheckIn("mgr", "server")
	r.CheckIn("dev_m", "zenbook")
	r.CheckIn("dev_f", "zenbook")

	got := r.Online()
	want := []string{"dev_f", "dev_m", "mgr"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Online() = %v, want %v", got, want)
	}

	e, ok := r.Get("dev_m")
	if !ok {
		t.Fatal("Get(dev_m) not found")
	}
	if e.Status != StatusOnline {
		t.Errorf("status = %q, want %q", e.Status, StatusOnline)
	}
	if e.DeviceID != "zenbook" {
		t.Errorf("device = %q, want zenbook", e.DeviceID)
	}
}

func TestCheckInReusesEntry(t *testing.T) {
	r, c := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	r.CheckIn("dev_m", "zenbook")
	r.CheckOut("dev_m")

	c.Advance(time.Hour)
	r.CheckIn("dev_m", "zenbook")

	e, _ := r.Get("dev_m")
	if e.Status != StatusOnline {
		t.Errorf("status = %q, want %q", e.Status, StatusOnline)
	}
	if !e.LastSeen.Equal(c.Now()) {
		t.Errorf("LastSeen = %v, want %v", e.LastSeen, c.Now())
	}
}

func TestCheckOut(t *testing.T) {
	r, _ := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	r.CheckIn("mgr", "server")
	r.CheckIn("dev_f", "zenbook")

	r.CheckOut("dev_f")

	e, ok := r.Get("dev_f")
	if !ok {
		t.Fatal("退勤後もエントリは残るべき")
	}
	if e.Status != StatusOffline {
		t.Errorf("status = %q, want %q", e.Status, StatusOffline)
	}

	got := r.Online()
	want := []string{"mgr"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Online() = %v, want %v", got, want)
	}

	// 未登録 ID の退勤は無視される。
	r.CheckOut("nobody")
	if _, ok := r.Get("nobody"); ok {
		t.Error("未登録 ID の CheckOut でエントリが作られた")
	}
}

func TestSetStatus(t *testing.T) {
	r, _ := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	r.CheckIn("dev_m", "zenbook")

	r.SetStatus("dev_m", StatusBusy)
	e, _ := r.Get("dev_m")
	if e.Status != StatusBusy {
		t.Fatalf("status = %q, want %q", e.Status, StatusBusy)
	}
	// Busy は Offline ではないので Online 一覧に含まれる。
	if got := r.Online(); !reflect.DeepEqual(got, []string{"dev_m"}) {
		t.Fatalf("Online() = %v, want [dev_m]", got)
	}

	r.SetStatus("dev_m", StatusBreak)
	if e, _ := r.Get("dev_m"); e.Status != StatusBreak {
		t.Fatalf("status = %q, want %q", e.Status, StatusBreak)
	}

	r.SetStatus("dev_m", StatusOffline)
	if e, _ := r.Get("dev_m"); e.Status != StatusOffline {
		t.Fatalf("status = %q, want %q", e.Status, StatusOffline)
	}
	if got := r.Online(); len(got) != 0 {
		t.Fatalf("Online() = %v, want empty", got)
	}

	// 未登録 ID は無視される。
	r.SetStatus("nobody", StatusOnline)
	if _, ok := r.Get("nobody"); ok {
		t.Error("未登録 ID の SetStatus でエントリが作られた")
	}
}

func TestHeartbeat(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	r, c := newTestRegistry(t0)
	r.CheckIn("mgr", "server")

	c.Advance(30 * time.Second)
	r.Heartbeat("mgr")

	e, _ := r.Get("mgr")
	if !e.LastSeen.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("LastSeen = %v, want %v", e.LastSeen, t0.Add(30*time.Second))
	}

	// 未登録 ID の heartbeat は何も作らない。
	r.Heartbeat("nobody")
	if _, ok := r.Get("nobody"); ok {
		t.Error("未登録 ID の Heartbeat でエントリが作られた")
	}
}

func TestExpire(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	r, c := newTestRegistry(t0)

	r.CheckIn("dev_m", "zenbook") // t0 のまま放置
	c.Advance(2 * time.Minute)
	r.CheckIn("dev_f", "zenbook") // 直前に heartbeat 相当
	r.CheckIn("mgr", "server")
	r.CheckOut("mgr") // 退勤済みは Expire 対象外

	expired := r.Expire(90 * time.Second)
	if !reflect.DeepEqual(expired, []string{"dev_m"}) {
		t.Fatalf("Expire() = %v, want [dev_m]", expired)
	}

	if e, _ := r.Get("dev_m"); e.Status != StatusOffline {
		t.Errorf("dev_m status = %q, want offline", e.Status)
	}
	if e, _ := r.Get("dev_f"); e.Status != StatusOnline {
		t.Errorf("dev_f status = %q, want online", e.Status)
	}

	// 2 回目は対象なし。
	if again := r.Expire(90 * time.Second); len(again) != 0 {
		t.Fatalf("2 回目の Expire() = %v, want empty", again)
	}
}

func TestSnapshot(t *testing.T) {
	r, _ := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	r.CheckIn("mgr", "server")
	r.CheckIn("chat", "zenbook")
	r.CheckIn("dev_m", "zenbook")

	snap := r.Snapshot()
	gotIDs := make([]string, len(snap))
	for i, e := range snap {
		gotIDs[i] = e.ID
	}
	want := []string{"chat", "dev_m", "mgr"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("Snapshot IDs = %v, want %v", gotIDs, want)
	}
}

// TestMarkResidentStaysOnlineAndSkipsExpire は常駐社員が timeout を超えても
// Expire で退勤にならず、online のまま留まることを確認する。
func TestMarkResidentStaysOnlineAndSkipsExpire(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	r, c := newTestRegistry(t0)

	r.MarkResident("mgr", "server")
	if !r.IsResident("mgr") {
		t.Fatal("MarkResident 後に IsResident(mgr) が false")
	}
	if e, ok := r.Get("mgr"); !ok || e.Status != StatusOnline {
		t.Fatalf("MarkResident 直後: ok=%v status=%v, want true/online", ok, e.Status)
	}

	// timeout を大きく超えて放置しても常駐社員は Expire の対象外。
	c.Advance(10 * time.Minute)
	if expired := r.Expire(90 * time.Second); len(expired) != 0 {
		t.Fatalf("Expire() = %v, want empty（常駐社員は対象外）", expired)
	}

	e, ok := r.Get("mgr")
	if !ok {
		t.Fatal("常駐社員のエントリが消えている")
	}
	if e.Status != StatusOnline {
		t.Errorf("status = %q, want %q", e.Status, StatusOnline)
	}
}

// TestResidentCheckOutIsIgnored は常駐社員が CheckOut されても offline にならないことを確認する。
func TestResidentCheckOutIsIgnored(t *testing.T) {
	r, _ := newTestRegistry(time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC))
	r.MarkResident("chat", "server")

	r.CheckOut("chat")

	e, ok := r.Get("chat")
	if !ok {
		t.Fatal("常駐社員のエントリが消えている")
	}
	if e.Status != StatusOnline {
		t.Errorf("CheckOut 後 status = %q, want %q（常駐社員は退勤しない）", e.Status, StatusOnline)
	}
	if got := r.Online(); !reflect.DeepEqual(got, []string{"chat"}) {
		t.Errorf("Online() = %v, want [chat]", got)
	}
}

// TestNonResidentStillExpires は常駐社員の除外が通常社員の Expire を壊していないことを確認する。
func TestNonResidentStillExpires(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	r, c := newTestRegistry(t0)

	r.MarkResident("mgr", "server")
	r.CheckIn("dev_m", "zenbook")

	c.Advance(2 * time.Minute)
	expired := r.Expire(90 * time.Second)
	if !reflect.DeepEqual(expired, []string{"dev_m"}) {
		t.Fatalf("Expire() = %v, want [dev_m]", expired)
	}
	if e, _ := r.Get("dev_m"); e.Status != StatusOffline {
		t.Errorf("dev_m status = %q, want offline", e.Status)
	}
	if e, _ := r.Get("mgr"); e.Status != StatusOnline {
		t.Errorf("mgr status = %q, want online", e.Status)
	}
}

// TestConcurrentAccess は -race 付きで並行アクセスの安全性を確認する。
func TestConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.CheckIn("dev_m", "zenbook")
			r.Heartbeat("dev_m")
			_ = r.Online()
			_ = r.Snapshot()
			r.SetStatus("dev_m", StatusBusy)
		}()
	}
	wg.Wait()
}
