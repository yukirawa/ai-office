package api

// economy.go は Phase 5（学の元帳・格差の観察）の API。
//
//   - GET  /api/ledger               全社員の残高
//   - GET  /api/ledger/:id/entries   元帳履歴
//   - GET  /api/economy/status       残高分布（格差の観察）
//
// 労働運動トリガー（§8 5.3）は「観察のみ」: 閾値を超えたら通知するだけで、行動は起こさない。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// handleLedgerAll は全社員の残高を返す（Phase 5.1）。
// 元帳の記録が無い社員（残高 0。例: chat）も含めて全社員を返す。
func (s *Server) handleLedgerAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	emps, err := s.store.Employees()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "社員一覧の取得に失敗しました")
		return
	}
	balances, err := s.economy.Balances(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "残高の取得に失敗しました")
		return
	}

	out := make(map[string]int, len(emps))
	for _, e := range emps {
		// Balances に無い社員はゼロ値 0 になる。
		out[e.ID] = balances[e.ID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": out})
}

// handleLedgerEntries は指定社員の元帳履歴を返す（Phase 5.1）。
func (s *Server) handleLedgerEntries(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := s.store.Employee(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "対象の社員がいません: "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "社員の取得に失敗しました")
		return
	}

	// limit は 1..200 に収める。範囲外（負値・0・巨大値）や非数値は既定 50。
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 200 {
			limit = n
		}
	}

	entries, err := s.store.Ledger(id, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "元帳の取得に失敗しました")
		return
	}

	type entry struct {
		Amount int    `json:"amount"`
		Reason string `json:"reason"`
		TS     string `json:"ts"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{Amount: e.Amount, Reason: e.Reason, TS: formatTS(e.TS)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee_id": id, "entries": out})
}

// economyStatus は残高分布の統計（格差の観察用）。
type economyStatus struct {
	Balances  map[string]int `json:"balances"`
	Min       int            `json:"min"`
	Max       int            `json:"max"`
	Spread    int            `json:"spread"`
	Total     int            `json:"total"`
	Threshold int            `json:"threshold"`
	Alert     bool           `json:"alert"`
}

// economyStatusSnapshot は現在の残高分布をまとめる。
func (s *Server) economyStatusSnapshot(ctx context.Context) economyStatus {
	st := economyStatus{Balances: map[string]int{}, Threshold: s.cfg.InequalityThreshold}
	balances, err := s.economy.Balances(ctx)
	if err != nil {
		return st
	}
	// /api/ledger と同じ母集団にする: 台帳に行が無い社員（残高 0。例: 日当制の chat）も
	// 含めて全社員で分布を取る。取得に失敗した場合は台帳のある社員だけで続行する。
	if emps, err := s.store.Employees(); err == nil {
		all := make(map[string]int, len(emps))
		for _, e := range emps {
			all[e.ID] = balances[e.ID]
		}
		balances = all
	}
	st.Balances = balances

	first := true
	for _, v := range balances {
		st.Total += v
		if first {
			st.Min, st.Max, first = v, v, false
			continue
		}
		if v < st.Min {
			st.Min = v
		}
		if v > st.Max {
			st.Max = v
		}
	}
	st.Spread = st.Max - st.Min
	st.Alert = st.Threshold > 0 && len(balances) > 1 && st.Spread >= st.Threshold
	return st
}

// handleEconomyStatus は残高分布を返す（Phase 5.3）。
func (s *Server) handleEconomyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.economyStatusSnapshot(r.Context()))
}

// ObserveInequality は格差を観察して、閾値を超えていれば通知する（Phase 5.3、観察のみ）。
// cron から定期的に呼ばれる。
func (s *Server) ObserveInequality(ctx context.Context) {
	st := s.economyStatusSnapshot(ctx)
	if !st.Alert {
		return
	}
	s.log.Warn("学の格差を観察しました（観察のみ・行動はしません）",
		"spread", st.Spread, "threshold", st.Threshold, "min", st.Min, "max", st.Max)
	if err := s.Notify(ctx, channelDefault, "system",
		fmt.Sprintf("【観察】学の格差が広がっています（最大 %d / 最小 %d / 差 %d）。", st.Max, st.Min, st.Spread)); err != nil {
		s.log.Warn("格差通知の投稿に失敗しました", "error", err)
	}
}
