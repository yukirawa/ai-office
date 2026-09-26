package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Relationship は relationships テーブルの 1 行（設計書 §5）。
// Affinity は -100〜+100、Trust は 0〜100 が意味上の範囲。
// store は値をそのまま保存する（範囲の調整は persona 側の責務）。
type Relationship struct {
	FromID    string
	ToID      string
	Affinity  int
	Trust     int
	UpdatedAt time.Time
}

const relationshipColumns = `from_id, to_id, affinity, trust, updated_at`

// UpsertRelationship は関係値を追加または更新する。UpdatedAt がゼロ値なら現在時刻。
func (s *Store) UpsertRelationship(r Relationship) error {
	if r.FromID == "" || r.ToID == "" {
		return errors.New("store: relationship の from_id / to_id が空です")
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = nowUTC()
	}
	if _, err := s.db.Exec(
		`INSERT INTO relationships (`+relationshipColumns+`) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(from_id, to_id) DO UPDATE SET
			affinity = excluded.affinity,
			trust = excluded.trust,
			updated_at = excluded.updated_at`,
		r.FromID, r.ToID, r.Affinity, r.Trust, formatTime(r.UpdatedAt),
	); err != nil {
		return fmt.Errorf("store: upsert relationship %s->%s: %w", r.FromID, r.ToID, err)
	}
	return nil
}

// Relationships は from_id から見た関係値を to_id 昇順で返す。
func (s *Store) Relationships(fromID string) ([]Relationship, error) {
	rows, err := s.db.Query(
		`SELECT `+relationshipColumns+` FROM relationships WHERE from_id = ? ORDER BY to_id`, fromID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: relationships %s: %w", fromID, err)
	}
	return scanRelationships(rows)
}

// AllRelationships は全関係値を (from_id, to_id) 昇順で返す。
func (s *Store) AllRelationships() ([]Relationship, error) {
	rows, err := s.db.Query(
		`SELECT ` + relationshipColumns + ` FROM relationships ORDER BY from_id, to_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: all relationships: %w", err)
	}
	return scanRelationships(rows)
}

// scanRelationships は複数行を Relationship に読み込む。
func scanRelationships(rows *sql.Rows) ([]Relationship, error) {
	defer rows.Close()

	out := make([]Relationship, 0, 16)
	for rows.Next() {
		var (
			r       Relationship
			updated string
		)
		if err := rows.Scan(&r.FromID, &r.ToID, &r.Affinity, &r.Trust, &updated); err != nil {
			return nil, fmt.Errorf("store: relationship 走査: %w", err)
		}
		r.UpdatedAt = parseTime(updated)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: relationship 走査: %w", err)
	}
	return out, nil
}
