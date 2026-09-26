package api

// economy.go は Phase 5（学の元帳・高級モデル購入・格差の観察）の API。
//
//   - GET  /api/ledger               全社員の残高
//   - GET  /api/ledger/:id/entries   元帳履歴
//   - POST /api/economy/purchase     高級モデルの購入（残高を消費してモデル差し替え）
//   - GET  /api/economy/status       残高分布（格差の観察）
//
// 労働運動トリガー（§8 5.3）は「観察のみ」: 閾値を超えたら通知するだけで、行動は起こさない。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// handleLedgerAll は全社員の残高を返す（Phase 5.1）。
func (s *Server) handleLedgerAll(w http.ResponseWriter, r *http.Request) {
	balances, err := s.economy.Balances(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "残高の取得に失敗しました")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": balances})
}

// handleLedgerEntries は指定社員の元帳履歴を返す（Phase 5.1）。
func (s *Server) handleLedgerEntries(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
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

// handlePurchase は高級モデルを購入する（Phase 5.2）。
// 残高から price を引き、対象エージェントのモデルを実行時に差し替える。
func (s *Server) handlePurchase(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EmployeeID string `json:"employee_id"`
		Model      string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON の解釈に失敗しました")
		return
	}

	id := strings.TrimSpace(body.EmployeeID)
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "employee_id は必須です")
		return
	}
	ag := s.employee(id)
	if ag == nil {
		writeJSONError(w, http.StatusNotFound, "対象の社員がいません: "+id)
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = s.cfg.PremiumModel
	}
	price := s.cfg.PremiumModelPrice
	if price <= 0 {
		writeJSONError(w, http.StatusBadRequest, "OFFICE_PREMIUM_MODEL_PRICE が未設定です")
		return
	}

	ctx := r.Context()
	balance, err := s.economy.Balance(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "残高の取得に失敗しました")
		return
	}
	if balance < price {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": "残高が足りません", "employee_id": id, "balance": balance, "price": price,
		})
		return
	}
	if err := s.economy.Debit(ctx, id, price, "高級モデル購入: "+model); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "支払いに失敗しました")
		return
	}
	ag.SetModel(model)

	if err := s.Notify(ctx, channelDefault, "system",
		fmt.Sprintf("%s が高級モデル %s を購入しました（-%d学）", id, model, price)); err != nil {
		s.log.Warn("購入通知の投稿に失敗しました", "error", err)
	}
	s.BroadcastState()

	newBalance, _ := s.economy.Balance(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"employee_id": id,
		"model":       model,
		"price":       price,
		"balance":     newBalance,
	})
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
