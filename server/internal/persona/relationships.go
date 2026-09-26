package persona

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/yukirawa/ai-office/server/internal/store"
)

// 関係値の意味上の範囲（設計書 §5 relationships）。
// store は値をそのまま保存するため、範囲の調整はこのパッケージの責務。
const (
	AffinityMin = -100
	AffinityMax = 100
	TrustMin    = 0
	TrustMax    = 100
)

// Service は関係値（affinity/trust）の読み書きを担う（設計書 §5 relationships / §8 4.2）。
//
// store の 1 呼び出しは直列化されるが、Adjust の read-modify-write は
// 複数呼び出しにまたがるため、mu で Service 単位に直列化してロストアップデートを防ぐ。
type Service struct {
	st *store.Store
	mu sync.Mutex
}

// NewService は store を使う関係値サービスを生成する。
func NewService(st *store.Store) *Service {
	return &Service{st: st}
}

// Get は fromID を起点とする関係値を返す。
func (s *Service) Get(ctx context.Context, fromID string) ([]store.Relationship, error) {
	if s == nil || s.st == nil {
		return nil, errors.New("persona: store が未設定です")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fromID = strings.TrimSpace(fromID)
	if fromID == "" {
		return nil, errors.New("persona: fromID が空です")
	}
	return s.st.Relationships(fromID)
}

// Adjust は fromID->toID の関係値を delta だけ動かして保存する。
// affinity は -100..+100、trust は 0..100 にクランプする。対象が無ければ 0 起点で作成する。
func (s *Service) Adjust(ctx context.Context, fromID, toID string, affinityDelta, trustDelta int) error {
	if s == nil || s.st == nil {
		return errors.New("persona: store が未設定です")
	}
	fromID = strings.TrimSpace(fromID)
	toID = strings.TrimSpace(toID)
	if fromID == "" || toID == "" {
		return errors.New("persona: fromID / toID は空にできません")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// read-modify-write を直列化する（並行呼び出しでのロストアップデート防止）。
	s.mu.Lock()
	defer s.mu.Unlock()

	affinity, trust := 0, 0
	rels, err := s.st.Relationships(fromID)
	if err != nil {
		return err
	}
	for _, r := range rels {
		if r.ToID == toID {
			affinity, trust = r.Affinity, r.Trust
			break
		}
	}

	return s.st.UpsertRelationship(store.Relationship{
		FromID:   fromID,
		ToID:     toID,
		Affinity: clampInt(affinity+affinityDelta, AffinityMin, AffinityMax),
		Trust:    clampInt(trust+trustDelta, TrustMin, TrustMax),
	})
}

// clampInt は v を [min, max] に収める。
func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
