package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yukirawa/ai-office/server/internal/config"
)

// Employee は employees テーブルの 1 行（設計書 §5）。
type Employee struct {
	ID          string
	Name        string
	Role        string
	Gender      string
	DeviceID    string
	PersonaJSON string
	CreatedAt   time.Time
}

// employeeColumns は employees の全カラム（SELECT 順序を 1 箇所に集約）。
const employeeColumns = `id, name, role, gender, device_id, persona_json, created_at`

// upsertEmployeeStmt は id をキーに upsert する。created_at は既存値を尊重する
// （再起動時の seed で作成日時を上書きしないため）。
const upsertEmployeeStmt = `INSERT INTO employees (` + employeeColumns + `)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		role = excluded.role,
		gender = excluded.gender,
		device_id = excluded.device_id,
		persona_json = excluded.persona_json`

// execer は *sql.DB と *sql.Tx の共通部分。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// UpsertEmployee は社員を追加または更新する。CreatedAt がゼロ値なら現在時刻を入れる。
func (s *Store) UpsertEmployee(e Employee) error {
	if e.ID == "" {
		return errors.New("store: employee id が空です")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = nowUTC()
	}
	return upsertEmployee(s.db, e)
}

// Employees は全社員を ID 昇順で返す。
func (s *Store) Employees() ([]Employee, error) {
	rows, err := s.db.Query(`SELECT ` + employeeColumns + ` FROM employees ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: employees 取得: %w", err)
	}
	defer rows.Close()

	var out []Employee
	for rows.Next() {
		e, err := scanEmployee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: employees 走査: %w", err)
	}
	return out, nil
}

// Employee は 1 名を返す。見つからない場合は sql.ErrNoRows をラップしたエラー。
func (s *Store) Employee(id string) (Employee, error) {
	row := s.db.QueryRow(`SELECT `+employeeColumns+` FROM employees WHERE id = ?`, id)
	e, err := scanEmployee(row)
	if err != nil {
		return Employee{}, fmt.Errorf("store: employee %s: %w", id, err)
	}
	return e, nil
}

// SeedEmployees は初期社員を upsert する。冪等なので起動のたびに呼んでよい。
func (s *Store) SeedEmployees(seeds []config.EmployeeSeed) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: seed begin: %w", err)
	}
	created := formatTime(nowUTC())
	for _, seed := range seeds {
		if seed.ID == "" {
			continue
		}
		e := Employee{
			ID:          seed.ID,
			Name:        seed.Name,
			Role:        seed.Role,
			Gender:      seed.Gender,
			DeviceID:    seed.DeviceID,
			PersonaJSON: seed.PersonaJSON,
			CreatedAt:   parseTime(created),
		}
		if err := upsertEmployee(tx, e); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: seed %s: %w", seed.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: seed commit: %w", err)
	}
	return nil
}

// upsertEmployee は exec 先（DB または Tx）に upsert を発行する。
func upsertEmployee(ex execer, e Employee) error {
	if _, err := ex.Exec(upsertEmployeeStmt,
		e.ID, e.Name, e.Role, e.Gender, e.DeviceID, e.PersonaJSON, formatTime(e.CreatedAt),
	); err != nil {
		return fmt.Errorf("store: upsert employee %s: %w", e.ID, err)
	}
	return nil
}

// scanEmployee は 1 行を Employee に読み込む。
func scanEmployee(sc scanner) (Employee, error) {
	var (
		e       Employee
		created string
	)
	if err := sc.Scan(&e.ID, &e.Name, &e.Role, &e.Gender, &e.DeviceID, &e.PersonaJSON, &created); err != nil {
		return Employee{}, err
	}
	e.CreatedAt = parseTime(created)
	return e, nil
}
