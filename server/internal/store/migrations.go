package store

import (
	"database/sql"
	"fmt"
)

// migrations は適用順のマイグレーション。各要素（=1 バージョン）は
// 実行する SQL 文のリスト。末尾に追加していく方式。
//
// 文はすべて冪等（IF NOT EXISTS）に書く。これにより、schema_version が
// 無い既存 DB（Phase 0 の成果物など）にも安全に適用できる。
var migrations = [][]string{
	// v1: 初期スキーマ（設計書 §5 のデータモデル）
	{
		`CREATE TABLE IF NOT EXISTS employees (
			id            TEXT PRIMARY KEY,
			name          TEXT NOT NULL DEFAULT '',
			role          TEXT NOT NULL DEFAULT '',
			gender        TEXT NOT NULL DEFAULT '',
			device_id     TEXT NOT NULL DEFAULT '',
			persona_json  TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL DEFAULT ''
		)`,
		// employee_id に FK は張らない。起動直後（社員 seed 前）のセッション開始や
		// テストで順序を気にせず書けるようにするための実装判断。
		`CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			employee_id TEXT NOT NULL DEFAULT '',
			device_id   TEXT NOT NULL DEFAULT '',
			started_at  TEXT NOT NULL DEFAULT '',
			ended_at    TEXT,
			end_reason  TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			channel  TEXT NOT NULL DEFAULT '',
			from_id  TEXT NOT NULL DEFAULT '',
			to_id    TEXT NOT NULL DEFAULT '',
			content  TEXT NOT NULL DEFAULT '',
			ts       TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS ledger (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			employee_id TEXT NOT NULL DEFAULT '',
			amount      INTEGER NOT NULL DEFAULT 0,
			reason      TEXT NOT NULL DEFAULT '',
			ts          TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS relationships (
			from_id    TEXT NOT NULL,
			to_id      TEXT NOT NULL,
			affinity   INTEGER NOT NULL DEFAULT 0,
			trust      INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (from_id, to_id)
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id          TEXT PRIMARY KEY,
			title       TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT '',
			assignee    TEXT NOT NULL DEFAULT '',
			created_by  TEXT NOT NULL DEFAULT '',
			result      TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT ''
		)`,
		// 検索用インデックス（設計書 §5: インデックス追加は自由）
		`CREATE INDEX IF NOT EXISTS idx_sessions_employee_open ON sessions (employee_id, ended_at)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages (channel, id)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_employee_id ON ledger (employee_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks (status, created_at)`,
	},
}

// migrate は未適用のマイグレーションを順に流す。
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: schema_version 作成: %w", err)
	}

	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: schema_version 読み取り: %w", err)
	}

	for i, stmts := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		if err := applyMigration(db, version, stmts); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration は 1 バージョン分の文をトランザクションで適用し、版を記録する。
func applyMigration(db *sql.DB, version int, stmts []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: migration v%d begin: %w", version, err)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration v%d: %w", version, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`,
		version, formatTime(nowUTC()),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: migration v%d 記録: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration v%d commit: %w", version, err)
	}
	return nil
}
