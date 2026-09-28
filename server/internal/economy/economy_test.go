package economy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yukirawa/ai-office/server/internal/store"
)

// newTestService は temp ファイルに SQLite を作って Service を返す。
// （:memory: ではなくファイルを使うのは、WAL など本番と同じ設定で確認するため。）
func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st)
}

func TestCreditDebitBalance(t *testing.T) {
	ctx := context.Background()
	e := newTestService(t)

	// 記録が無い社員の残高は 0。
	if got, err := e.Balance(ctx, "dev_m"); err != nil || got != 0 {
		t.Fatalf("Balance(dev_m) = %d, %v; want 0, nil", got, err)
	}

	if err := e.Credit(ctx, "dev_m", 500, "月給"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := e.Debit(ctx, "dev_m", 120, "備品購入"); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	got, err := e.Balance(ctx, "dev_m")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got != 380 {
		t.Fatalf("Balance(dev_m) = %d, want 380", got)
	}

	// 別社員は独立。
	if err := e.Credit(ctx, "dev_f", 350, "月給"); err != nil {
		t.Fatalf("Credit dev_f: %v", err)
	}
	balances, err := e.Balances(ctx)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	want := map[string]int{"dev_m": 380, "dev_f": 350}
	if len(balances) != len(want) {
		t.Fatalf("Balances = %v, want %v", balances, want)
	}
	for id, v := range want {
		if balances[id] != v {
			t.Errorf("Balances[%s] = %d, want %d", id, balances[id], v)
		}
	}
}

func TestInvalidAmount(t *testing.T) {
	ctx := context.Background()
	e := newTestService(t)

	for _, amount := range []int{0, -1} {
		if err := e.Credit(ctx, "dev_m", amount, "x"); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("Credit(%d) err = %v, want ErrInvalidAmount", amount, err)
		}
		if err := e.Debit(ctx, "dev_m", amount, "x"); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("Debit(%d) err = %v, want ErrInvalidAmount", amount, err)
		}
	}

	// 不正入力は元帳に残らない。
	if got, _ := e.Balance(ctx, "dev_m"); got != 0 {
		t.Errorf("Balance = %d, want 0", got)
	}
}

func TestPayWages(t *testing.T) {
	ctx := context.Background()
	e := newTestService(t)

	wages := map[string]int{
		"mgr":   16, // 500/30 の切り捨て
		"dev_m": 11, // 350/30 の切り捨て
		"dev_f": 11,
		"chat":  0, // 日当制: スキップされる
	}
	if err := e.PayWages(ctx, wages, "日割り給与"); err != nil {
		t.Fatalf("PayWages: %v", err)
	}

	balances, err := e.Balances(ctx)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	if balances["mgr"] != 16 || balances["dev_m"] != 11 || balances["dev_f"] != 11 {
		t.Fatalf("Balances = %v, want mgr=16 dev_m=11 dev_f=11", balances)
	}
	if _, ok := balances["chat"]; ok {
		t.Errorf("chat は amount<=0 なので記録されないはず: %v", balances)
	}

	// 2 回目の支給で残高が積み上がる。
	if err := e.PayWages(ctx, wages, "日割り給与"); err != nil {
		t.Fatalf("PayWages(2): %v", err)
	}
	if got, _ := e.Balance(ctx, "mgr"); got != 32 {
		t.Errorf("Balance(mgr) = %d, want 32", got)
	}
}

func TestPayWagesEmpty(t *testing.T) {
	e := newTestService(t)
	// 支給対象なしでもエラーにならない。
	if err := e.PayWages(context.Background(), map[string]int{"chat": 0}, "日割り給与"); err != nil {
		t.Fatalf("PayWages: %v", err)
	}
	if err := e.PayWages(context.Background(), nil, "日割り給与"); err != nil {
		t.Fatalf("PayWages(nil): %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	e := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := e.Credit(ctx, "dev_m", 100, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Credit err = %v, want context.Canceled", err)
	}
	if _, err := e.Balance(ctx, "dev_m"); !errors.Is(err, context.Canceled) {
		t.Errorf("Balance err = %v, want context.Canceled", err)
	}
}
