package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Task は tasks テーブルの 1 行（設計書 §5）。
// Status は pending|assigned|working|review|done|failed。
type Task struct {
	ID          string
	Title       string
	Description string
	Status      string
	Assignee    string
	CreatedBy   string
	Result      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const taskColumns = `id, title, description, status, assignee, created_by, result, created_at, updated_at`

// InsertTask はタスクを追加する。CreatedAt / UpdatedAt がゼロ値なら現在時刻。
func (s *Store) InsertTask(t Task) error {
	if t.ID == "" {
		return errors.New("store: task id が空です")
	}
	now := nowUTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Description, t.Status, t.Assignee, t.CreatedBy, t.Result,
		formatTime(t.CreatedAt), formatTime(t.UpdatedAt),
	); err != nil {
		return fmt.Errorf("store: insert task %s: %w", t.ID, err)
	}
	return nil
}

// UpdateTask は status と result を更新し、updated_at を現在時刻にする。
// 対象が無ければ sql.ErrNoRows をラップしたエラー。
func (s *Store) UpdateTask(id, status, result string) error {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = ?, result = ?, updated_at = ? WHERE id = ?`,
		status, result, formatTime(nowUTC()), id,
	)
	if err != nil {
		return fmt.Errorf("store: update task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update task %s rows: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: update task %s: %w", id, sql.ErrNoRows)
	}
	return nil
}

// Task は 1 件返す。無ければ sql.ErrNoRows をラップしたエラー。
func (s *Store) Task(id string) (Task, error) {
	row := s.db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if err != nil {
		return Task{}, fmt.Errorf("store: task %s: %w", id, err)
	}
	return t, nil
}

// Tasks は直近 limit 件を新しい順で返す。status が空文字なら全ステータス、
// limit が 0 以下なら全件。
func (s *Store) Tasks(status string, limit int) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: tasks 取得: %w", err)
	}
	defer rows.Close()

	out := make([]Task, 0, 16)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tasks 走査: %w", err)
	}
	return out, nil
}

// scanTask は 1 行を Task に読み込む。
func scanTask(sc scanner) (Task, error) {
	var (
		t                Task
		created, updated string
	)
	if err := sc.Scan(&t.ID, &t.Title, &t.Description, &t.Status, &t.Assignee,
		&t.CreatedBy, &t.Result, &created, &updated); err != nil {
		return Task{}, err
	}
	t.CreatedAt = parseTime(created)
	t.UpdatedAt = parseTime(updated)
	return t, nil
}
