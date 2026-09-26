// Package gh は GitHub 連携（設計書 §2 の gh、Phase 3）の入り口。
//
// Phase 1 では webhook 受信のハンドラを置くだけで、外部呼び出しは行わない。
// Phase 3 で署名検証（Webhook secret）と PR 作成をここに実装する。
package gh

import (
	"net/http"
)

// Handler は GitHub webhook（/webhook/github）のハンドラを返す。
// 現時点では 501 Not Implemented を返すスタブ。
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"not_implemented","message":"GitHub webhook は Phase 3 で実装予定です"}`))
	}
}
