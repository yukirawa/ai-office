package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session は sessions テーブルの 1 行（設計書 §5）。EndedAt が nil なら勤務中。
type Session struct {
	ID         string
	EmployeeID string
	DeviceID   string
	StartedAt  time.Time
	EndedAt    *time.Time
	EndReason  string
}

const sessionColumns = `id, employee_id, device_id, started_at, ended_at, end_reason`

// StartSession は出勤セッションを作成する。StartedAt は現在時刻。
func (s *Store) StartSession(id, employeeID, deviceID string) error {
	if id == "" {
		return errors.New("store: session id が空です")
	}
	if _, err := s.db.Exec(
		`INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, NULL, '')`,
		id, employeeID, deviceID, formatTime(nowUTC()),
	); err != nil {
		return fmt.Errorf("store: start session %s: %w", id, err)
	}
	return nil
}

// EndSession は指定セッションを退勤にする。既に終了済みなら何もしない（冪等）。
func (s *Store) EndSession(id, reason string) error {
	if id == "" {
		return errors.New("store: session id が空です")
	}
	if _, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`,
		formatTime(nowUTC()), reason, id,
	); err != nil {
		return fmt.Errorf("store: end session %s: %w", id, err)
	}
	return nil
}

// EndActiveSessions はある社員の未終了セッションをすべて終了する
// （再接続や heartbeat timeout 時に古いセッションを閉じる用途）。
func (s *Store) EndActiveSessions(employeeID, reason string) error {
	if _, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, end_reason = ? WHERE employee_id = ? AND ended_at IS NULL`,
		formatTime(nowUTC()), reason, employeeID,
	); err != nil {
		return fmt.Errorf("store: end active sessions %s: %w", employeeID, err)
	}
	return nil
}

// ActiveSession は社員の勤務中セッションを返す。
// 無ければ sql.ErrNoRows をラップしたエラー。
func (s *Store) ActiveSession(employeeID string) (Session, error) {
	row := s.db.QueryRow(
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE employee_id = ? AND ended_at IS NULL
		 ORDER BY started_at DESC, id DESC LIMIT 1`, employeeID,
	)
	sess, err := scanSession(row)
	if err != nil {
		return Session{}, fmt.Errorf("store: active session %s: %w", employeeID, err)
	}
	return sess, nil
}

// scanSession は 1 行を Session に読み込む。ended_at は NULL 可。
func scanSession(sc scanner) (Session, error) {
	var (
		sess      Session
		started   string
		ended     sql.NullString
		endReason string
	)
	if err := sc.Scan(&sess.ID, &sess.EmployeeID, &sess.DeviceID, &started, &ended, &endReason); err != nil {
		return Session{}, err
	}
	sess.StartedAt = parseTime(started)
	if ended.Valid {
		t := parseTime(ended.String)
		sess.EndedAt = &t
	}
	sess.EndReason = endReason
	return sess, nil
}
