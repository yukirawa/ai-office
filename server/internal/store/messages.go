package store

import (
	"fmt"
	"time"
)

// Message は messages テーブルの 1 行（設計書 §5）。チャンネル投稿や DM を表す。
type Message struct {
	ID      int64
	Channel string
	FromID  string
	ToID    string
	Content string
	TS      time.Time
}

const messageColumns = `id, channel, from_id, to_id, content, ts`

// InsertMessage はメッセージを追加し、採番された ID を返す。TS がゼロ値なら現在時刻。
func (s *Store) InsertMessage(m Message) (int64, error) {
	if m.TS.IsZero() {
		m.TS = nowUTC()
	}
	res, err := s.db.Exec(
		`INSERT INTO messages (channel, from_id, to_id, content, ts) VALUES (?, ?, ?, ?, ?)`,
		m.Channel, m.FromID, m.ToID, m.Content, formatTime(m.TS),
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: insert message id: %w", err)
	}
	return id, nil
}

// Messages は直近 limit 件を古い順（時系列順）で返す。
// channel が空文字なら全チャンネル、limit が 0 以下なら全件。
func (s *Store) Messages(channel string, limit int) ([]Message, error) {
	q := `SELECT ` + messageColumns + ` FROM messages`
	var args []any
	if channel != "" {
		q += ` WHERE channel = ?`
		args = append(args, channel)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: messages 取得: %w", err)
	}
	defer rows.Close()

	out := make([]Message, 0, 16)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: messages 走査: %w", err)
	}

	// DESC で取ったので古い順に反転して返す。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// scanMessage は 1 行を Message に読み込む。
func scanMessage(sc scanner) (Message, error) {
	var (
		m  Message
		ts string
	)
	if err := sc.Scan(&m.ID, &m.Channel, &m.FromID, &m.ToID, &m.Content, &ts); err != nil {
		return Message{}, err
	}
	m.TS = parseTime(ts)
	return m, nil
}
