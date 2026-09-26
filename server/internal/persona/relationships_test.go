package persona

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yukirawa/ai-office/server/internal/store"
)

// openTestService は temp ディレクトリの SQLite を使う Service を作る（store_test.go と同じ流儀）。
func openTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewService(st)
}

// relFor は fromID 起点の関係値から toID の組を取り出す。
func relFor(t *testing.T, s *Service, fromID, toID string) store.Relationship {
	t.Helper()
	rels, err := s.Get(context.Background(), fromID)
	if err != nil {
		t.Fatalf("Get(%s): %v", fromID, err)
	}
	for _, r := range rels {
		if r.ToID == toID {
			return r
		}
	}
	t.Fatalf("relationship %s->%s が見つかりません: %+v", fromID, toID, rels)
	return store.Relationship{}
}

func TestServiceGetEmpty(t *testing.T) {
	s := openTestService(t)
	rels, err := s.Get(context.Background(), "mgr")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("Get(空) = %+v, want 0 件", rels)
	}
}

func TestAdjustCreatesFromZero(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	if err := s.Adjust(ctx, "mgr", "dev_m", 5, 10); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	r := relFor(t, s, "mgr", "dev_m")
	if r.Affinity != 5 || r.Trust != 10 {
		t.Fatalf("作成直後の値 = %+v, want affinity=5 trust=10", r)
	}
	if r.UpdatedAt.IsZero() {
		t.Error("UpdatedAt がゼロ値です（保存時に現在時刻が入るべき）")
	}
}

func TestAdjustAccumulates(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	// 0 → 2/1 → 4/2 → 1/0（-3/-2 で減算）。
	for _, step := range []struct {
		dA, dT, wantA, wantT int
	}{
		{2, 1, 2, 1},
		{2, 1, 4, 2},
		{-3, -2, 1, 0},
	} {
		if err := s.Adjust(ctx, "mgr", "dev_m", step.dA, step.dT); err != nil {
			t.Fatalf("Adjust(%d,%d): %v", step.dA, step.dT, err)
		}
		if r := relFor(t, s, "mgr", "dev_m"); r.Affinity != step.wantA || r.Trust != step.wantT {
			t.Fatalf("Adjust(%d,%d) 後 = %+v, want affinity=%d trust=%d",
				step.dA, step.dT, r, step.wantA, step.wantT)
		}
	}
}

func TestAdjustClampsUpper(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	if err := s.Adjust(ctx, "mgr", "dev_m", 1000, 1000); err != nil {
		t.Fatalf("Adjust(1000,1000): %v", err)
	}
	if r := relFor(t, s, "mgr", "dev_m"); r.Affinity != AffinityMax || r.Trust != TrustMax {
		t.Fatalf("上限クランプ後 = %+v, want affinity=%d trust=%d", r, AffinityMax, TrustMax)
	}
	// すでに上限でもさらに加算して範囲内に留まる。
	if err := s.Adjust(ctx, "mgr", "dev_m", 50, 50); err != nil {
		t.Fatalf("Adjust(50,50): %v", err)
	}
	if r := relFor(t, s, "mgr", "dev_m"); r.Affinity != AffinityMax || r.Trust != TrustMax {
		t.Fatalf("再加算後 = %+v, want 上限のまま", r)
	}
}

func TestAdjustClampsLower(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	if err := s.Adjust(ctx, "mgr", "dev_m", -1000, -1000); err != nil {
		t.Fatalf("Adjust(-1000,-1000): %v", err)
	}
	if r := relFor(t, s, "mgr", "dev_m"); r.Affinity != AffinityMin || r.Trust != TrustMin {
		t.Fatalf("下限クランプ後 = %+v, want affinity=%d trust=%d", r, AffinityMin, TrustMin)
	}
}

func TestAdjustValidatesIDs(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	for _, tc := range []struct{ from, to string }{
		{"", "dev_m"},
		{"mgr", ""},
		{"   ", "dev_m"},
	} {
		if err := s.Adjust(ctx, tc.from, tc.to, 1, 1); err == nil {
			t.Errorf("Adjust(from=%q, to=%q) = nil, want エラー", tc.from, tc.to)
		}
	}
}

func TestGetSortedByToID(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	if err := s.Adjust(ctx, "mgr", "dev_m", 1, 1); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if err := s.Adjust(ctx, "mgr", "dev_f", 2, 2); err != nil {
		t.Fatalf("Adjust: %v", err)
	}

	rels, err := s.Get(ctx, "mgr")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rels) != 2 || rels[0].ToID != "dev_f" || rels[1].ToID != "dev_m" {
		t.Fatalf("to_id 昇順でない: %+v", rels)
	}
}

// 並行 Adjust でもロストアップデートしない（Service 内で直列化）。
func TestAdjustConcurrent(t *testing.T) {
	s := openTestService(t)
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Adjust(ctx, "mgr", "dev_m", 1, 1); err != nil {
				t.Errorf("Adjust: %v", err)
			}
		}()
	}
	wg.Wait()

	if r := relFor(t, s, "mgr", "dev_m"); r.Affinity != n || r.Trust != n {
		t.Fatalf("並行加算後 = %+v, want affinity=%d trust=%d", r, n, n)
	}
}
