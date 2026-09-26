// Package economy は社内通貨「学」の元帳（設計書 §2 の economy）を扱う。
//
// 実際の永続化は store の ledger テーブルに委ね、ここでは収支の符号や
// 給与支払いのバッチ処理といった業務ルールだけを持つ。
package economy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/yukirawa/ai-office/server/internal/store"
)

// ErrInvalidAmount は金額が正でない場合のエラー。errors.Is で判定できる。
var ErrInvalidAmount = errors.New("economy: amount は正の値である必要があります")

// Service は学の元帳操作を提供する。
type Service struct {
	st *store.Store
}

// New は store を backing とする Service を生成する。
func New(st *store.Store) *Service {
	return &Service{st: st}
}

// Credit は収入を記録する。amount は正の値でなければならない。
func (e *Service) Credit(ctx context.Context, employeeID string, amount int, reason string) error {
	if amount <= 0 {
		return fmt.Errorf("economy: credit %s: %w (amount=%d)", employeeID, ErrInvalidAmount, amount)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.st.InsertLedger(store.LedgerEntry{
		EmployeeID: employeeID,
		Amount:     amount,
		Reason:     reason,
	}); err != nil {
		return fmt.Errorf("economy: credit %s: %w", employeeID, err)
	}
	return nil
}

// Debit は支出を記録する。amount は正の値で受け取り、ledger には負値で保存する。
func (e *Service) Debit(ctx context.Context, employeeID string, amount int, reason string) error {
	if amount <= 0 {
		return fmt.Errorf("economy: debit %s: %w (amount=%d)", employeeID, ErrInvalidAmount, amount)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.st.InsertLedger(store.LedgerEntry{
		EmployeeID: employeeID,
		Amount:     -amount,
		Reason:     reason,
	}); err != nil {
		return fmt.Errorf("economy: debit %s: %w", employeeID, err)
	}
	return nil
}

// Balance は社員の残高を返す。
func (e *Service) Balance(ctx context.Context, employeeID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return e.st.Balance(employeeID)
}

// Balances は全社員の残高を返す。
func (e *Service) Balances(ctx context.Context) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.st.AllBalances()
}

// PayWages は日割り給与を一括で支給する。wageByEmployee は社員 ID→金額で、
// amount <= 0 の社員はスキップする。reason は元帳の理由に使う。
// 1 トランザクションで記録するため、途中失敗時は誰にも支払われない。
func (e *Service) PayWages(ctx context.Context, wageByEmployee map[string]int, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// map の反復順は不定なので、決定的な順序（ID 昇順）に固定する。
	ids := make([]string, 0, len(wageByEmployee))
	for id, wage := range wageByEmployee {
		if wage > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	entries := make([]store.LedgerEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, store.LedgerEntry{
			EmployeeID: id,
			Amount:     wageByEmployee[id],
			Reason:     reason,
		})
	}
	if err := e.st.InsertLedgerBatch(entries); err != nil {
		return fmt.Errorf("economy: pay wages: %w", err)
	}
	return nil
}
