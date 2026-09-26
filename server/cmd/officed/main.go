// Command officed は ai-office のサーバ（頭脳・記憶・調整）を起動する。
//
// 設計書 §2 の officed に対応する。Phase 0 で presence/WS を、Phase 1 で
// SQLite・LLM ラッパー・mgr エージェント・日割り給与 cron を配線する。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/api"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/economy"
	"github.com/yukirawa/ai-office/server/internal/gh"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
	"github.com/yukirawa/ai-office/server/internal/presence"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// shutdownTimeout は HTTP サーバの graceful shutdown の猶予。
const shutdownTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		// run 内でログ済みだが、起動失敗は必ず非ゼロで終える。
		fmt.Fprintln(os.Stderr, "officed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	// ---- 記憶（SQLite） ----
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Warn("DB のクローズに失敗しました", "error", cerr)
		}
	}()
	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		return fmt.Errorf("初期社員の投入に失敗しました: %w", err)
	}
	seedRelationships(st, logger)

	// ---- 在席 ----
	reg := presence.NewRegistry()

	// ---- 経済 ----
	econ := economy.New(st)

	// ---- LLM ----
	client := buildLLM(cfg, logger)

	// ---- 調整（mgr エージェント） ----
	srv := api.New(api.Deps{
		Cfg:      cfg,
		Store:    st,
		Presence: reg,
		Economy:  econ,
		Logger:   logger,
	})
	mgr := agents.NewManager(
		config.EmployeeManagerID,
		loadPersona(st, config.EmployeeManagerID, logger),
		client,
		srv,
		agentOptions(cfg, logger),
	)

	// ---- 手足の操作（Phase 2: dev エージェント） ----
	// dev エージェントはサーバー側の頭脳で、実際のファイル操作や GitHub 操作は
	// worker（Zenbook）に task_assign して実行させる。
	devM := agents.NewDevAgent(
		config.EmployeeDevMID, loadPersona(st, config.EmployeeDevMID, logger),
		client, srv, srv, srv, srv, mgr, agentOptions(cfg, logger),
	)
	devF := agents.NewDevAgent(
		config.EmployeeDevFID, loadPersona(st, config.EmployeeDevFID, logger),
		client, srv, srv, srv, srv, mgr, agentOptions(cfg, logger),
	)
	mgr.SetAssignees(devM, devF)
	mgr.SetTaskUpdater(srv)
	srv.SetManager(mgr)

	// ---- GitHub 連携（Phase 3） ----
	// 設定が揃っているときだけ有効化する。worker 不在時はサーバーが PR を作る。
	if cfg.GitHubEnabled() {
		ghClient, err := gh.New(gh.Config{
			AppID:          cfg.GitHubAppID,
			InstallationID: cfg.GitHubInstallationID,
			PrivateKeyPEM:  cfg.GitHubPrivateKeyPEM,
		})
		if err != nil {
			logger.Error("GitHub 連携を初期化できませんでした", "error", err)
		} else {
			srv.SetGitHub(ghClient)
			logger.Info("GitHub 連携を有効化しました",
				"app_id", cfg.GitHubAppID,
				"installation_id", cfg.GitHubInstallationID,
				"repo", cfg.GitHubRepo,
			)
		}
	} else {
		logger.Info("GitHub 連携は無効です（GITHUB_APP_ID / GITHUB_INSTALLATION_ID / 秘密鍵が未設定）")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- バックグラウンド処理 ----
	go srv.RunReaper(ctx)
	go mgr.Run(ctx)
	go devM.Run(ctx)
	go devF.Run(ctx)

	// ---- 日割り給与 cron（Phase 1.4） ----
	cronScheduler := startPayrollCron(ctx, cfg, st, econ, srv, logger)
	defer cronScheduler.Stop()

	// ---- HTTP / WebSocket ----
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("officed を起動しました",
			"addr", cfg.Addr,
			"db", cfg.DBPath,
			"llm_provider", cfg.LLMProvider,
			"heartbeat_timeout", cfg.HeartbeatTimeout,
			"payroll_cron", cfg.PayrollCron,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("停止信号を受け取りました。graceful shutdown します")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("officed を停止しました")
	return nil
}

// newLogger はログレベル名から slog.Logger を作る。
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// buildLLM は設定に応じた LLM クライアントを作る。
// API キーが無い場合は mock にフォールバックし、オフラインでも動作させる。
func buildLLM(cfg *config.Config, logger *slog.Logger) llm.Client {
	switch strings.ToLower(strings.TrimSpace(cfg.LLMProvider)) {
	case "anthropic":
		if cfg.AnthropicAPIKey == "" {
			logger.Warn("ANTHROPIC_API_KEY が未設定のため mock プロバイダで動作します")
			return mockLLM()
		}
		logger.Info("LLM プロバイダ: anthropic", "model", cfg.LLMModel, "base_url", cfg.AnthropicBaseURL)
		return llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL)
	default:
		logger.Info("LLM プロバイダ: mock（外部 API を呼びません）")
		return mockLLM()
	}
}

// agentOptions は mgr / dev 共通の Options を設定から組み立てる。
func agentOptions(cfg *config.Config, logger *slog.Logger) agents.Options {
	return agents.Options{
		MaxTurns:    cfg.MaxAgentTurns,
		TokenBudget: cfg.TokenBudget,
		Channel:     "#会議室",
		Logger:      logger,
	}
}

// mockLLM は Phase 1 の計画フェーズ用の擬似応答を返す。
func mockLLM() llm.Client {
	return llm.NewMock(
		"【計画】1) 現状とゴールを確認 2) 作業を分解して担当（dev_m / dev_f）を割当 3) レビュー観点を定義 4) 実行は Phase 2 以降で対応。",
	)
}

// loadPersona は employees.persona_json からペルソナを読む。無ければ空のペルソナ。
func loadPersona(st *store.Store, id string, logger *slog.Logger) persona.Persona {
	e, err := st.Employee(id)
	if err != nil {
		logger.Warn("ペルソナの取得に失敗しました", "employee_id", id, "error", err)
		return persona.Persona{}
	}
	return persona.Load(e.PersonaJSON)
}

// seedRelationships は初回起動時のみ、TUI の関係表示用に最小限の関係値を入れる。
// 本格的な関係値システムは Phase 4.2。ここでは空だと右ペインが寂しいための初期データ。
func seedRelationships(st *store.Store, logger *slog.Logger) {
	existing, err := st.AllRelationships()
	if err != nil {
		logger.Warn("関係値の取得に失敗しました", "error", err)
		return
	}
	if len(existing) > 0 {
		return
	}
	now := time.Now().UTC()
	seeds := []store.Relationship{
		{FromID: config.EmployeeManagerID, ToID: config.EmployeeDevFID, Affinity: 15, Trust: 70, UpdatedAt: now},
		{FromID: config.EmployeeManagerID, ToID: config.EmployeeDevMID, Affinity: 3, Trust: 60, UpdatedAt: now},
		{FromID: config.EmployeeDevFID, ToID: config.EmployeeManagerID, Affinity: 12, Trust: 65, UpdatedAt: now},
		{FromID: config.EmployeeDevMID, ToID: config.EmployeeManagerID, Affinity: 5, Trust: 62, UpdatedAt: now},
	}
	for _, r := range seeds {
		if err := st.UpsertRelationship(r); err != nil {
			logger.Warn("関係値の投入に失敗しました", "from", r.FromID, "to", r.ToID, "error", err)
			return
		}
	}
	logger.Info("初期の関係値を投入しました", "count", len(seeds))
}

// startPayrollCron は日割り給与の cron を開始する（Phase 1.4）。
// cron 式とタイムゾーンは設定（環境変数）から取る。
func startPayrollCron(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	econ *economy.Service,
	srv *api.Server,
	logger *slog.Logger,
) *cron.Cron {
	loc, err := time.LoadLocation(cfg.PayrollTimezone)
	if err != nil {
		logger.Warn("給与 cron のタイムゾーンが不正なため Local を使います",
			"payroll_tz", cfg.PayrollTimezone, "error", err)
		loc = time.Local
	}

	c := cron.New(cron.WithLocation(loc))
	_, err = c.AddFunc(cfg.PayrollCron, func() {
		runPayroll(ctx, st, econ, srv, loc, logger)
	})
	if err != nil {
		logger.Error("給与 cron の登録に失敗しました", "cron", cfg.PayrollCron, "error", err)
		return c
	}
	c.Start()
	logger.Info("日割り給与 cron を開始しました", "cron", cfg.PayrollCron, "tz", loc.String())
	return c
}

// runPayroll は全社員に日割り給与を支給し、会議室へ通知する。
func runPayroll(
	ctx context.Context,
	st *store.Store,
	econ *economy.Service,
	srv *api.Server,
	loc *time.Location,
	logger *slog.Logger,
) {
	emps, err := st.Employees()
	if err != nil {
		logger.Error("給与支給: 社員一覧の取得に失敗しました", "error", err)
		return
	}

	wages := make(map[string]int, len(emps))
	for _, e := range emps {
		if w := config.DailyWage(e.ID); w > 0 {
			wages[e.ID] = w
		}
	}
	if len(wages) == 0 {
		return
	}

	day := time.Now().In(loc).Format("2006-01-02")
	reason := "日割り給与 " + day
	if err := econ.PayWages(ctx, wages, reason); err != nil {
		logger.Error("給与支給に失敗しました", "error", err)
		return
	}

	if err := srv.Notify(ctx, "#会議室", "system",
		fmt.Sprintf("日割り給与を支給しました（%s、対象 %d 名）", day, len(wages))); err != nil {
		logger.Warn("給与通知の投稿に失敗しました", "error", err)
	}
	srv.BroadcastState()
	logger.Info("日割り給与を支給しました", "day", day, "count", len(wages))
}
