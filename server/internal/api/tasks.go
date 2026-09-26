package api

// tasks.go は Phase 2/3 のタスク実行を api 側でつなぐ。
//
// api は agents パッケージの次のインターフェースを実装する:
//   - agents.Dispatcher     … task_assign の送信と task_result の待ち合わせ
//   - agents.TaskUpdater    … tasks テーブルの状態更新
//   - agents.RemoteExecutor … worker 不在時のサーバー側 GitHub 実行
//
// あわせて GitHub webhook をタスク化する（§13.6, Phase 3.3）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/gh"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// waiter は task_result を待つ 1 件分の待機。
type waiter struct {
	employeeID string
	ch         chan agents.Result
}

// SetGitHub は GitHub クライアントを設定する（Phase 3）。nil なら remote は無効。
func (s *Server) SetGitHub(c gh.Client) {
	s.mu.Lock()
	s.ghClient = c
	s.mu.Unlock()
}

// github は現在の GitHub クライアントを返す（未設定なら nil）。
func (s *Server) github() gh.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ghClient
}

// Dispatch は agents.Dispatcher の実装。指定社員の worker へ task_assign を送り、
// 結果待ちの待機を登録する。remote モードでは installation token を発行して埋める。
func (s *Server) Dispatch(ctx context.Context, employeeID string, assign agents.TaskAssign) error {
	taskID := strings.TrimSpace(assign.TaskID)
	if taskID == "" {
		return errors.New("api: task_id が指定されていません")
	}

	cl := s.hub.byEmployee(employeeID)
	if cl == nil {
		return fmt.Errorf("api: %s の worker が接続していません", employeeID)
	}

	if assign.Mode == "remote" && assign.Remote != nil && strings.TrimSpace(assign.Remote.Token) == "" {
		ghc := s.github()
		if ghc == nil {
			return errors.New("api: GitHub 連携が設定されていません")
		}
		token, err := ghc.InstallationToken(ctx)
		if err != nil {
			return fmt.Errorf("api: installation token の発行に失敗しました: %w", err)
		}
		assign.Remote.Token = token
	}

	// task_result が送信直後に返っても取りこぼさないよう、送信前に待機を登録する。
	w := &waiter{employeeID: employeeID, ch: make(chan agents.Result, 1)}
	s.waitersMu.Lock()
	s.waiters[taskID] = w
	s.waitersMu.Unlock()

	if !s.hub.sendJSONTo(cl, taskAssignMsg{Type: "task_assign", TaskID: taskID, Payload: assign}) {
		s.waitersMu.Lock()
		if s.waiters[taskID] == w {
			delete(s.waiters, taskID)
		}
		s.waitersMu.Unlock()
		return fmt.Errorf("api: %s への送信に失敗しました（送信バッファ満杯）", employeeID)
	}

	s.log.Info("task_assign を送信しました",
		"task_id", taskID, "employee_id", employeeID, "mode", assign.Mode)
	return nil
}

// Await は agents.Dispatcher の実装。task_result が届くまで待つ。
func (s *Server) Await(ctx context.Context, taskID string) (agents.Result, error) {
	s.waitersMu.Lock()
	w := s.waiters[taskID]
	s.waitersMu.Unlock()
	if w == nil {
		return agents.Result{}, fmt.Errorf("api: task %s の待機が登録されていません", taskID)
	}

	select {
	case res := <-w.ch:
		return res, nil
	case <-ctx.Done():
		s.waitersMu.Lock()
		if s.waiters[taskID] == w {
			delete(s.waiters, taskID)
		}
		s.waitersMu.Unlock()
		return agents.Result{}, ctx.Err()
	}
}

// UpdateTask は agents.TaskUpdater の実装。
// タスクを含む office_state をブロードキャストして TUI に反映させる。
func (s *Server) UpdateTask(ctx context.Context, taskID, status, result string) error {
	if err := s.store.UpdateTask(taskID, status, result); err != nil {
		return err
	}
	s.log.Info("タスク状態を更新しました", "task_id", taskID, "status", status)
	s.BroadcastState()
	return nil
}

// AssignTask は agents の任意インターフェース taskAssigner の実装。
// tasks.assignee を実行担当に更新し、office_state（タスクペイン）を配信する。
func (s *Server) AssignTask(ctx context.Context, taskID, assignee string) error {
	if err := s.store.SetTaskAssignee(taskID, assignee); err != nil {
		return err
	}
	s.log.Info("タスクの担当者を記録しました", "task_id", taskID, "assignee", assignee)
	s.BroadcastState()
	return nil
}

// ExecuteRemote は agents.RemoteExecutor の実装。
// worker 不在時にサーバー側で GitHub 操作（branch 作成 → commit → PR）を行う
// （§2「PCオフライン時：頭脳だけ働き、作業はGitHub上で完結」）。
func (s *Server) ExecuteRemote(ctx context.Context, spec agents.RemoteSpec) (agents.Result, error) {
	ghc := s.github()
	if ghc == nil {
		return agents.Result{}, errors.New("api: GitHub 連携が設定されていません")
	}
	if strings.TrimSpace(spec.Repo) == "" {
		return agents.Result{}, errors.New("api: repo が指定されていません")
	}

	files := make([]gh.File, 0, len(spec.Files))
	for _, f := range spec.Files {
		files = append(files, gh.File{Path: f.Path, Content: f.Content})
	}

	res, err := ghc.CreatePullRequest(ctx, gh.PRRequest{
		Repo:       spec.Repo,
		BaseBranch: spec.BaseBranch,
		Branch:     spec.Branch,
		Title:      spec.Title,
		Body:       spec.Body,
		Files:      files,
	})
	if err != nil {
		s.log.Error("サーバー側の PR 作成に失敗しました", "repo", spec.Repo, "branch", spec.Branch, "error", err)
		return agents.Result{Status: "failed", Summary: "PR 作成に失敗しました: " + err.Error()}, nil
	}
	s.log.Info("サーバー側で PR を作成しました", "repo", spec.Repo, "number", res.Number, "url", res.HTMLURL)
	return agents.Result{Status: "done", Summary: fmt.Sprintf("PR を作成しました: %s", res.HTMLURL)}, nil
}

// deliverResult は task_result を待機中の Await に渡す。
func (s *Server) deliverResult(res agents.Result) {
	s.waitersMu.Lock()
	w := s.waiters[res.TaskID]
	if w != nil {
		delete(s.waiters, res.TaskID)
	}
	s.waitersMu.Unlock()
	if w == nil {
		s.log.Debug("待機の無い task_result を無視しました", "task_id", res.TaskID)
		return
	}
	select {
	case w.ch <- res:
	default:
	}
}

// failWaiters は切断した worker に割り当てていた待機を失敗で埋める。
// これにより Await がタイムアウトまでブロックしない。
func (s *Server) failWaiters(employeeID, reason string) {
	s.waitersMu.Lock()
	var failed []*waiter
	for id, w := range s.waiters {
		if w.employeeID == employeeID {
			failed = append(failed, w)
			delete(s.waiters, id)
		}
	}
	s.waitersMu.Unlock()

	for _, w := range failed {
		select {
		case w.ch <- agents.Result{Status: "failed", Summary: "worker が切断しました: " + reason}:
		default:
		}
	}
}

// CreateTaskInput はタスク作成の入力。
type CreateTaskInput struct {
	Title       string
	Description string
	From        string
	Mode        string // "local"（既定） | "remote"
	Repo        string
	BaseBranch  string
}

// CreateTask はタスクを保存し、mgr の受信箱へ渡す。戻り値はタスク ID。
func (s *Server) CreateTask(ctx context.Context, in CreateTaskInput) (string, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return "", errors.New("api: title は必須です")
	}
	if strings.TrimSpace(in.From) == "" {
		in.From = "owner"
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "local"
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	task := store.Task{
		ID:          id,
		Title:       in.Title,
		Description: in.Description,
		Status:      "pending",
		Assignee:    config.EmployeeManagerID,
		CreatedBy:   in.From,
		Mode:        mode,
		Repo:        in.Repo,
		BaseBranch:  in.BaseBranch,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.InsertTask(task); err != nil {
		return "", err
	}

	m := s.Manager()
	if m == nil {
		return id, nil
	}
	if ok := m.Post(agents.Task{
		ID:          id,
		Title:       in.Title,
		Description: in.Description,
		From:        in.From,
		Mode:        mode,
		Repo:        in.Repo,
		BaseBranch:  in.BaseBranch,
	}); !ok {
		return "", errors.New("api: mgr の受信箱が満杯です")
	}

	s.log.Info("タスクを受け付けました",
		"task_id", id, "title", in.Title, "from", in.From, "mode", mode, "repo", in.Repo)
	return id, nil
}

// handleListTasks はタスク一覧を返す（検証用の拡張。?status=&limit=）。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	tasks, err := s.store.Tasks(status, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "タスク一覧の取得に失敗しました")
		return
	}

	type taskResponse struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Assignee  string `json:"assignee"`
		CreatedBy string `json:"created_by"`
		Result    string `json:"result"`
		Mode      string `json:"mode"`
		Repo      string `json:"repo"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	out := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskResponse{
			ID: t.ID, Title: t.Title, Status: t.Status, Assignee: t.Assignee,
			CreatedBy: t.CreatedBy, Result: t.Result, Mode: t.Mode, Repo: t.Repo,
			CreatedAt: formatTS(t.CreatedAt), UpdatedAt: formatTS(t.UpdatedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWebhookEvent は GitHub webhook をタスク化する（§13.6, Phase 3.3）。
func (s *Server) handleWebhookEvent(ctx context.Context, ev gh.WebhookEvent) {
	s.log.Info("webhook を受信しました",
		"kind", ev.Kind, "action", ev.Action, "repo", ev.Repo, "number", ev.Number)

	if ev.Kind == "ping" {
		return
	}

	// Phase 3 では issue の起票を remote タスク（PR 作成）に変換する。
	if ev.Kind != "issues" || ev.Action != "opened" {
		s.log.Debug("webhook: 対応しないイベントを無視しました", "kind", ev.Kind, "action", ev.Action)
		return
	}

	repo := strings.TrimSpace(ev.Repo)
	if repo == "" {
		repo = strings.TrimSpace(s.cfg.GitHubRepo)
	}
	if repo == "" {
		s.log.Warn("webhook: repo が特定できないためタスク化をスキップします", "issue", ev.Number)
		return
	}

	_, err := s.CreateTask(ctx, CreateTaskInput{
		Title:       fmt.Sprintf("Issue #%d: %s", ev.Number, ev.Title),
		Description: ev.Body,
		From:        fromGitHubSender(ev.Sender),
		Mode:        "remote",
		Repo:        repo,
		BaseBranch:  s.cfg.GitHubBaseBranch,
	})
	if err != nil {
		s.log.Error("webhook からのタスク作成に失敗しました",
			"issue", ev.Number, "repo", repo, "error", err)
	}
}

// fromGitHubSender は webhook の sender を from_id に整形する。
func fromGitHubSender(sender string) string {
	sender = strings.TrimSpace(sender)
	if sender == "" {
		return "github"
	}
	return "github:" + sender
}
