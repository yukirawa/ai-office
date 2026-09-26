// Package store は SQLite への永続化を担当する。
//
// 設計書 §5 のデータモデル（固定）に対応する。タイムスタンプは RFC3339 UTC の
// 文字列で保存する。並行書き込み時の SQLITE_BUSY を避けるため接続数は 1 に固定する。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// pure-Go の SQLite ドライバ。driver 名は "sqlite"。
	_ "modernc.org/sqlite"
)

// driverName は database/sql に登録されるドライバ名。
const driverName = "sqlite"

// Store は SQLite 接続をラップする。すべてのメソッドは並行呼び出し可能
// （接続数 1 のため実質直列化される）。
type Store struct {
	db *sql.DB
}

// Open は path の SQLite を開き、PRAGMA を設定してマイグレーションを実行する。
// 親ディレクトリが無ければ作成する。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: DB パスが空です")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: ディレクトリ作成 %s: %w", dir, err)
		}
	}

	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLITE_BUSY を避けるため接続を 1 本に固定する。
	// PRAGMA は接続単位で効くので、接続が閉じないよう Idle も 1 にする
	// （そうしないと新しい接続で foreign_keys などが既定値に戻る）。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// 接続を 1 本に固定した後に PRAGMA を流す（同じ接続に確実に効かせるため）。
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %q: %w", p, err)
		}
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close は DB を閉じる。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// scanner は *sql.Row と *sql.Rows の共通部分。
type scanner interface {
	Scan(dest ...any) error
}

// nowUTC は現在時刻を UTC で返す。
func nowUTC() time.Time { return time.Now().UTC() }

// formatTime は時刻を RFC3339 UTC 文字列に変換する。
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseTime は RFC3339 文字列を UTC の time.Time に戻す。不正な値はゼロ値。
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
