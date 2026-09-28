// Package presence は AI 社員の出退勤と在席状況を管理する。
//
// 設計書 §6.2 の presence パッケージに対応する。Registry は in-memory で
// 社員の在席を持ち、WebSocket の hello / heartbeat / bye から更新される。
package presence

import (
	"sort"
	"sync"
	"time"
)

// Status は在席状況。設計書 §6.2 の enum。
type Status string

const (
	StatusOnline  Status = "online"
	StatusBusy    Status = "busy"
	StatusBreak   Status = "break"
	StatusOffline Status = "offline"
)

// Employee は在席中の社員 1 名の状態。
type Employee struct {
	ID       string
	DeviceID string
	Status   Status
	// LastSeen は最後に heartbeat（または出退勤）を受けた時刻。
	LastSeen time.Time
}

// Registry は社員の在席状況を保持する。RWMutex で保護し、並行アクセス安全。
type Registry struct {
	mu        sync.RWMutex
	employees map[string]*Employee
	// residents はサーバー常駐の社員（mgr / chat）。heartbeat を送らないため
	// Expire の対象から外す（設計書 §3 の「鰯常駐」社員）。
	residents map[string]struct{}
	// now は現在時刻を返す関数。テストで差し替えられるようにフィールド化している。
	now func() time.Time
}

// NewRegistry は空の Registry を生成する。
func NewRegistry() *Registry {
	return &Registry{
		employees: make(map[string]*Employee),
		residents: make(map[string]struct{}),
		now:       time.Now,
	}
}

// MarkResident はサーバー常駐の社員（mgr / chat）を在席として登録する。
// 常駐社員はクライアント接続や heartbeat を持たないため、Expire で退勤にしない。
func (r *Registry) MarkResident(id, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.residents == nil {
		r.residents = make(map[string]struct{})
	}
	r.residents[id] = struct{}{}

	now := r.now()
	if e, ok := r.employees[id]; ok {
		e.DeviceID = deviceID
		e.Status = StatusOnline
		e.LastSeen = now
		return
	}
	r.employees[id] = &Employee{
		ID:       id,
		DeviceID: deviceID,
		Status:   StatusOnline,
		LastSeen: now,
	}
}

// IsResident はサーバー常駐社員かを返す。
func (r *Registry) IsResident(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.residents[id]
	return ok
}

// CheckIn は出勤を記録する。未登録の社員なら新規作成する。
func (r *Registry) CheckIn(id, deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if e, ok := r.employees[id]; ok {
		e.DeviceID = deviceID
		e.Status = StatusOnline
		e.LastSeen = now
		return
	}
	r.employees[id] = &Employee{
		ID:       id,
		DeviceID: deviceID,
		Status:   StatusOnline,
		LastSeen: now,
	}
}

// CheckOut は退勤を記録する。エントリは残し、Status を Offline にして
// 「退勤」として見えるようにする（設計書 §7.4 の TUI 表示）。
// 未登録の ID は無視する。
func (r *Registry) CheckOut(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.employees[id]
	if !ok {
		return
	}
	// サーバー常駐社員（mgr / chat）は退勤にしない。
	if _, resident := r.residents[id]; resident {
		return
	}
	e.Status = StatusOffline
	e.LastSeen = r.now()
}

// Heartbeat は既存エントリの LastSeen のみを更新する。
// Offline から Online へは戻さない（退勤済みの社員を heartbeat だけで復帰させない）。
// 未登録の ID は無視する。
func (r *Registry) Heartbeat(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.employees[id]
	if !ok {
		return
	}
	e.LastSeen = r.now()
}

// SetStatus は在席状況を変更する。未登録の ID は無視する。
func (r *Registry) SetStatus(id string, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.employees[id]
	if !ok {
		return
	}
	e.Status = s
}

// Get は社員 1 名の状態を返す。第 2 戻り値は存在有無。
func (r *Registry) Get(id string) (Employee, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.employees[id]
	if !ok {
		return Employee{}, false
	}
	return *e, true
}

// Online は Status != Offline の社員 ID を昇順で返す（設計書 §4.2 の office.online）。
func (r *Registry) Online() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.employees))
	for id, e := range r.employees {
		if e.Status != StatusOffline {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Snapshot は追跡中の全社員を ID 昇順で返す。
func (r *Registry) Snapshot() []Employee {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Employee, 0, len(r.employees))
	for _, e := range r.employees {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Expire は Status != Offline かつ LastSeen が now-timeout より古い社員を
// Offline に落とし、その ID を昇順で返す。タイムアウト判定のタイマーから呼ばれる。
func (r *Registry) Expire(timeout time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := r.now().Add(-timeout)
	var expired []string
	for id, e := range r.employees {
		if e.Status == StatusOffline {
			continue
		}
		// サーバー常駐社員（mgr / chat）は heartbeat を送らないので対象外。
		if _, resident := r.residents[id]; resident {
			continue
		}
		if e.LastSeen.Before(cutoff) {
			e.Status = StatusOffline
			expired = append(expired, id)
		}
	}
	sort.Strings(expired)
	return expired
}
