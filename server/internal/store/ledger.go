package store

import (
	"fmt"
	"time"
)

// LedgerEntry は ledger テーブルの 1 行（設計書 §5）。
// Amount は正=収入、負=支出。
type LedgerEntry struct {
	ID         int64
	EmployeeID string
	Amount     int
	Reason     string
	TS         time.Time
}

const ledgerColumns = `id, employee_id, amount, reason, ts`

// InsertLedger は元帳に 1 件追加する。TS がゼロ値なら現在時刻。
func (s *Store) InsertLedger(e LedgerEntry) error {
	if e.TS.IsZero() {
		e.TS = nowUTC()
	}
	if _, err := s.db.Exec(
		`INSERT INTO ledger (employee_id, amount, reason, ts) VALUES (?, ?, ?, ?)`,
		e.EmployeeID, e.Amount, e.Reason, formatTime(e.TS),
	); err != nil {
		return fmt.Errorf("store: insert ledger %s: %w", e.EmployeeID, err)
	}
	return nil
}

// InsertLedgerBatch は複数の元帳エントリを 1 トランザクションで追加する。
// 給与支払いのように「全員分支払うか、誰も支払わないか」を保証したい用途で使う。
func (s *Store) InsertLedgerBatch(entries []LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: ledger batch begin: %w", err)
	}
	now := nowUTC()
	for _, e := range entries {
		ts := e.TS
		if ts.IsZero() {
			ts = now
		}
		if _, err := tx.Exec(
			`INSERT INTO ledger (employee_id, amount, reason, ts) VALUES (?, ?, ?, ?)`,
			e.EmployeeID, e.Amount, e.Reason, formatTime(ts),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: ledger batch insert %s: %w", e.EmployeeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: ledger batch commit: %w", err)
	}
	return nil
}

// Balance は社員の残高（amount の合計）を返す。記録が無ければ 0。
func (s *Store) Balance(employeeID string) (int, error) {
	var total int
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(amount), 0) FROM ledger WHERE employee_id = ?`, employeeID,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: balance %s: %w", employeeID, err)
	}
	return total, nil
}

// Ledger は社員の直近 limit 件を古い順（時系列順）で返す。
// limit が 0 以下なら全件。
func (s *Store) Ledger(employeeID string, limit int) ([]LedgerEntry, error) {
	q := `SELECT ` + ledgerColumns + ` FROM ledger WHERE employee_id = ? ORDER BY id DESC`
	args := []any{employeeID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ledger 取得 %s: %w", employeeID, err)
	}
	defer rows.Close()

	out := make([]LedgerEntry, 0, 16)
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ledger 走査 %s: %w", employeeID, err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AllBalances は全社員の残高を返す。記録が無い社員は含まれない。
func (s *Store) AllBalances() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT employee_id, SUM(amount) FROM ledger GROUP BY employee_id`)
	if err != nil {
		return nil, fmt.Errorf("store: all balances: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var (
			id    string
			total int
		)
		if err := rows.Scan(&id, &total); err != nil {
			return nil, fmt.Errorf("store: all balances 走査: %w", err)
		}
		out[id] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: all balances: %w", err)
	}
	return out, nil
}

// scanLedgerEntry は 1 行を LedgerEntry に読み込む。
func scanLedgerEntry(sc scanner) (LedgerEntry, error) {
	var (
		e  LedgerEntry
		ts string
	)
	if err := sc.Scan(&e.ID, &e.EmployeeID, &e.Amount, &e.Reason, &ts); err != nil {
		return LedgerEntry{}, err
	}
	e.TS = parseTime(ts)
	return e, nil
}
