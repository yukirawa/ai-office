// Package api は HTTP / WebSocket のハンドラと、接続中クライアントへの
// ブロードキャストを担当する。
//
// 設計書 §4 のプロトコルと §4.3 のエンドポイントを実装する。また agents.Notifier
// を実装し、エージェントからの投稿・状態公開を WebSocket へ流す。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/economy"
	"github.com/yukirawa/ai-office/server/internal/gh"
	"github.com/yukirawa/ai-office/server/internal/presence"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// Deps は Server の依存。New で受け取る。
type Deps struct {
	Cfg      *config.Config
	Store    *store.Store
	Presence *presence.Registry
	Economy  *economy.Service
	// Manager は mgr エージェント。SetManager で後から差し込める（循環依存回避のため）。
	Manager *agents.Manager
	Logger  *slog.Logger
}

// Server は HTTP/WS サーバの状態を保持する。
type Server struct {
	cfg      *config.Config
	store    *store.Store
	presence *presence.Registry
	economy  *economy.Service
	log      *slog.Logger

	hub *hub

	mu          sync.RWMutex
	manager     *agents.Manager
	agentStates map[string]string
	ghClient    gh.Client

	// waiters は task_result を待つ DevAgent への受け渡し（tasks.go）。
	waitersMu sync.Mutex
	waiters   map[string]*waiter
}

// New は Server を生成する。
func New(d Deps) *Server {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:         d.Cfg,
		store:       d.Store,
		presence:    d.Presence,
		economy:     d.Economy,
		log:         logger,
		hub:         newHub(),
		manager:     d.Manager,
		agentStates: make(map[string]string),
		waiters:     make(map[string]*waiter),
	}
	return s
}

// SetManager は mgr エージェントを後から差し込む。
// Server が agents.Notifier を実装し、Manager がそれを必要とするため、
// 生成順の循環をこの setter で断ち切る。
func (s *Server) SetManager(m *agents.Manager) {
	s.mu.Lock()
	s.manager = m
	s.mu.Unlock()
}

// Manager は現在の mgr エージェントを返す（未設定なら nil）。
func (s *Server) Manager() *agents.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manager
}

// Notify は agents.Notifier の実装。メッセージを永続化して notice を配信する。
func (s *Server) Notify(ctx context.Context, channel, fromID, text string) error {
	if strings.TrimSpace(channel) == "" {
		channel = channelDefault
	}
	if strings.TrimSpace(fromID) == "" {
		fromID = notifyFromDefault
	}
	ts := time.Now().UTC()
	if _, err := s.store.InsertMessage(store.Message{
		Channel: channel,
		FromID:  fromID,
		Content: text,
		TS:      ts,
	}); err != nil {
		return fmt.Errorf("api: メッセージ保存に失敗しました: %w", err)
	}
	s.hub.broadcast(noticeMsg{
		Type:    "notice",
		Channel: channel,
		From:    fromID,
		Text:    text,
		TS:      formatTS(ts),
	})
	return nil
}

// SetAgentState は agents.Notifier の実装。社員のエージェント状態を公開する。
func (s *Server) SetAgentState(employeeID, state string) {
	s.mu.Lock()
	s.agentStates[employeeID] = state
	s.mu.Unlock()
	s.BroadcastState()
}

// BroadcastState はオフィス全体のスナップショットを全クライアントへ送る。
func (s *Server) BroadcastState() {
	s.hub.broadcast(s.snapshot(context.Background()))
}

// snapshot は現在のオフィス状態を組み立てる。
func (s *Server) snapshot(ctx context.Context) officeStateMsg {
	msg := officeStateMsg{
		Type:          "office_state",
		Online:        nonNil(s.presence.Online()),
		Employees:     []employeeWire{},
		Ledger:        map[string]int{},
		Relationships: []relationshipWire{},
		TS:            formatTS(time.Now().UTC()),
	}

	emps, err := s.store.Employees()
	if err != nil {
		s.log.Error("社員一覧の取得に失敗しました", "error", err)
	} else {
		s.mu.RLock()
		states := make(map[string]string, len(s.agentStates))
		for k, v := range s.agentStates {
			states[k] = v
		}
		s.mu.RUnlock()

		for _, e := range emps {
			status := string(presence.StatusOffline)
			if p, ok := s.presence.Get(e.ID); ok {
				status = string(p.Status)
			}
			msg.Employees = append(msg.Employees, employeeWire{
				ID:     e.ID,
				Name:   e.Name,
				Role:   e.Role,
				Status: status,
				State:  states[e.ID],
			})
		}
	}

	if bal, err := s.economy.Balances(ctx); err != nil {
		s.log.Warn("残高の取得に失敗しました", "error", err)
	} else if bal != nil {
		msg.Ledger = bal
	}

	if rels, err := s.store.AllRelationships(); err != nil {
		s.log.Warn("関係値の取得に失敗しました", "error", err)
	} else {
		for _, r := range rels {
			msg.Relationships = append(msg.Relationships, relationshipWire{
				FromID:   r.FromID,
				ToID:     r.ToID,
				Affinity: r.Affinity,
				Trust:    r.Trust,
			})
		}
	}
	return msg
}

// Handler はルーティング済みの http.Handler を返す。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	// §4.3 /healthz
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	// §4.3 /ws
	r.Get("/ws", s.handleWS)

	// §4.3 /api/employees
	r.Get("/api/employees", s.handleEmployees)

	// §4.3 /api/ledger/:id
	r.Get("/api/ledger/{id}", s.handleLedger)

	// 関係値（設計書 §5 の relationships。TUI 右ペイン用の拡張エンドポイント）
	r.Get("/api/relationships/{id}", s.handleRelationships)

	// オーナーからのタスク投入（Phase 2 で実行まで配線）
	r.Post("/api/tasks", s.handleCreateTask)
	r.Get("/api/tasks", s.handleListTasks)

	// §4.3 /webhook/github
	// Phase 3: 署名検証してタスク化する。secret は cfg（env GITHUB_WEBHOOK_SECRET）。
	r.Post("/webhook/github", gh.Handler(s.cfg.GitHubWebhookSecret, s.handleWebhookEvent))

	return r
}

// HandleReaper は heartbeat 途絶を検出して退勤扱いにする goroutine。
// 設計書 §6.1 presence、§4 の定期送信（推奨 30 秒）に対応する。
func (s *Server) RunReaper(ctx context.Context) {
	interval := s.cfg.HeartbeatTimeout / 3
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log.Info("在席タイムアウト監視を開始しました",
		"timeout", s.cfg.HeartbeatTimeout, "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired := s.presence.Expire(s.cfg.HeartbeatTimeout)
			if len(expired) == 0 {
				continue
			}
			for _, id := range expired {
				if err := s.store.EndActiveSessions(id, "timeout"); err != nil {
					s.log.Warn("タイムアウトセッションの終了に失敗しました", "employee_id", id, "error", err)
				}
				s.log.Info("応答が無いため退勤扱いにしました", "employee_id", id)
				if err := s.Notify(ctx, channelDefault, notifyFromDefault,
					fmt.Sprintf("%s は応答が無いため退勤扱いにしました（タイムアウト）", id)); err != nil {
					s.log.Warn("タイムアウト通知の投稿に失敗しました", "error", err)
				}
			}
			s.BroadcastState()
		}
	}
}

// ---- REST ハンドラ ----

// employeeResponse は /api/employees の 1 要素。
type employeeResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Gender   string `json:"gender"`
	DeviceID string `json:"device_id"`
	Status   string `json:"status"`
	State    string `json:"state"`
	LastSeen string `json:"last_seen,omitempty"`
}

func (s *Server) handleEmployees(w http.ResponseWriter, r *http.Request) {
	emps, err := s.store.Employees()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "社員一覧の取得に失敗しました")
		return
	}

	s.mu.RLock()
	states := make(map[string]string, len(s.agentStates))
	for k, v := range s.agentStates {
		states[k] = v
	}
	s.mu.RUnlock()

	out := make([]employeeResponse, 0, len(emps))
	for _, e := range emps {
		status := string(presence.StatusOffline)
		lastSeen := ""
		if p, ok := s.presence.Get(e.ID); ok {
			status = string(p.Status)
			if !p.LastSeen.IsZero() {
				lastSeen = formatTS(p.LastSeen)
			}
		}
		out = append(out, employeeResponse{
			ID:       e.ID,
			Name:     e.Name,
			Role:     e.Role,
			Gender:   e.Gender,
			DeviceID: e.DeviceID,
			Status:   status,
			State:    states[e.ID],
			LastSeen: lastSeen,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	balance, err := s.economy.Balance(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "残高の取得に失敗しました")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"employee_id": id,
		"balance":     balance,
	})
}

func (s *Server) handleRelationships(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rels, err := s.store.Relationships(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "関係値の取得に失敗しました")
		return
	}
	type relResponse struct {
		FromID    string `json:"from_id"`
		ToID      string `json:"to_id"`
		Affinity  int    `json:"affinity"`
		Trust     int    `json:"trust"`
		UpdatedAt string `json:"updated_at,omitempty"`
	}
	out := make([]relResponse, 0, len(rels))
	for _, rel := range rels {
		out = append(out, relResponse{
			FromID:    rel.FromID,
			ToID:      rel.ToID,
			Affinity:  rel.Affinity,
			Trust:     rel.Trust,
			UpdatedAt: formatTS(rel.UpdatedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateTask はオーナーからのタスク投入を受け付ける（§6.1、Phase 2 で実行まで配線）。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		From        string `json:"from"`
		Mode        string `json:"mode"`
		Repo        string `json:"repo"`
		BaseBranch  string `json:"base_branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON の解釈に失敗しました")
		return
	}

	id, err := s.CreateTask(r.Context(), CreateTaskInput{
		Title:       body.Title,
		Description: body.Description,
		From:        body.From,
		Mode:        body.Mode,
		Repo:        body.Repo,
		BaseBranch:  body.BaseBranch,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "必須") {
			status = http.StatusBadRequest
		}
		if strings.Contains(err.Error(), "満杯") {
			status = http.StatusServiceUnavailable
		}
		writeJSONError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"task_id":  id,
		"status":   "pending",
		"assignee": config.EmployeeManagerID,
	})
}

// ---- ヘルパ ----

// formatTS は時刻を RFC3339 UTC 文字列にする（§4 の ts 表記）。
func formatTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
