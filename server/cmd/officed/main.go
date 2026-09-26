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
	// 実 API キー/モデルの聴通確認（サーバは起動しない）。
	if len(os.Args) > 1 && os.Args[1] == "--llm-check" {
		if err := runLLMCheck(); err != nil {
			fmt.Fprintln(os.Stderr, "llm-check:", err)
			os.Exit(1)
		}
		return
	}

	// 実 API キーで利用可能なモデル一覧を確認する（トークン消費なし）。
	if len(os.Args) > 1 && os.Args[1] == "--llm-models" {
		if err := runLLMModels(); err != nil {
			fmt.Fprintln(os.Stderr, "llm-models:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// run 内でログ済みだが、起動失敗は必ず非ゼロで終える。
		fmt.Fprintln(os.Stderr, "officed:", err)
		os.Exit(1)
	}
}

// runLLMModels は実 API キーで利用可能なモデル一覧を表示する（--llm-models）。
// Anthropic 以外（mock 等）ではエラーを返す。
func runLLMModels() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	if err := requireLLMKey(cfg); err != nil {
		return err
	}

	client := buildLLM(cfg, logger)
	type modelLister interface {
		Models(ctx context.Context) ([]string, error)
	}
	lister, ok := client.(modelLister)
	if !ok {
		return fmt.Errorf("プロバイダ %q ではモデル一覧を取得できません（OFFICE_LLM_PROVIDER=anthropic で試してください）", cfg.LLMProvider)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := lister.Models(ctx)
	if err != nil {
		return fmt.Errorf("モデル一覧の取得に失敗しました: %w", err)
	}

	fmt.Printf("利用可能なモデル (%d 件):\n", len(models))
	for _, m := range models {
		fmt.Printf("  - %s\n", m)
	}
	fmt.Println("\n使いたいモデル名を OFFICE_LLM_MODEL に設定してください。")
	return nil
}

// requireLLMKey は選んだプロバイダの API キーが無ければエラーを返す（診断コマンド用）。
// mock はキー不要なのでエラーにしない。
func requireLLMKey(cfg *config.Config) error {
	switch strings.ToLower(strings.TrimSpace(cfg.LLMProvider)) {
	case "anthropic":
		if strings.TrimSpace(cfg.AnthropicAPIKey) == "" {
			return errors.New("ANTHROPIC_API_KEY が未設定です（環境変数 / .env / secrets/anthropic.key のいずれかに設定してください）")
		}
	case "deepseek":
		if strings.TrimSpace(cfg.DeepSeekAPIKey) == "" {
			return errors.New("DEEPSEEK_API_KEY が未設定です（環境変数 / .env / secrets/deepseek.key のいずれかに設定してください）")
		}
	}
	return nil
}

// runLLMCheck は実 API キー/モデルの聴通を 1 回の Chat で確認する（--llm-check）。
// サーバを起動せずに使えるので、キーの動作テストの入口になる。
func runLLMCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	if err := requireLLMKey(cfg); err != nil {
		return err
	}

	client := buildLLM(cfg, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := client.Chat(ctx, llm.Request{
		Model:     cfg.LLMModel,
		System:    "接続確認です。簡潔に日本語で返してください。",
		Messages:  []llm.Message{{Role: "user", Content: "『pong』とだけ返してください。"}},
		MaxTokens: 32,
	})
	if err != nil {
		return fmt.Errorf("LLM 呼び出しに失敗しました: %w", err)
	}

	model := strings.TrimSpace(resp.Model)
	if model == "" {
		model = cfg.LLMModel
	}
	fmt.Printf("provider=%s model=%s elapsed=%s tokens(in/out)=%d/%d\nreply=%s\n",
		cfg.LLMProvider, model, time.Since(start).Round(time.Millisecond),
		resp.InputTokens, resp.OutputTokens, strings.TrimSpace(resp.Text))
	return nil
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
		LLM:      client,
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
	// Phase 4.2: 関係値（persona.Service は agents.RelationshipUpdater を満たす）。
	mgr.SetRelationships(persona.NewService(st))
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

	// ---- 雑談役（Phase 4.3。Ollama は使わずサーバーの LLM で応答する） ----
	chat := agents.NewChatAgent(
		config.EmployeeChatID,
		loadPersona(st, config.EmployeeChatID, logger),
		client,
		srv,
		agentOptions(cfg, logger),
	)
	srv.SetChat(chat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- バックグラウンド処理 ----
	go srv.RunReaper(ctx)
	go mgr.Run(ctx)
	go devM.Run(ctx)
	go devF.Run(ctx)
	go chat.Run(ctx)

	// ---- 日割り給与 cron（Phase 1.4） ----
	cronScheduler := startPayrollCron(ctx, cfg, st, econ, srv, logger)
	defer cronScheduler.Stop()

	// ---- 雑談 cron（Phase 4.4） ----
	chatScheduler := startChatCron(ctx, cfg, chat, logger)
	defer chatScheduler.Stop()

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
		anthropic := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, llm.WithTimeout(cfg.LLMTimeout))
		logger.Info("LLM プロバイダ: anthropic", "model", cfg.LLMModel, "client", anthropic.String())
		return anthropic
	case "deepseek":
		if cfg.DeepSeekAPIKey == "" {
			logger.Warn("DEEPSEEK_API_KEY が未設定のため mock プロバイダで動作します")
			return mockLLM()
		}
		deepseek := llm.NewDeepSeek(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, llm.WithTimeout(cfg.LLMTimeout))
		logger.Info("LLM プロバイダ: deepseek", "model", cfg.LLMModel, "client", deepseek.String())
		return deepseek
	default:
		logger.Info("LLM プロバイダ: mock（外部 API を呼びません）")
		return mockLLM()
	}
}

// agentOptions は mgr / dev / chat 共通の Options を設定から組み立てる。
func agentOptions(cfg *config.Config, logger *slog.Logger) agents.Options {
	return agents.Options{
		MaxTurns:    cfg.MaxAgentTurns,
		TokenBudget: cfg.TokenBudget,
		Model:       cfg.LLMModel,
		MaxTokens:   cfg.LLMMaxTokens,
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
	if !cfg.PayrollCronEnabled() {
		logger.Info("日割り給与 cron は無効です", "cron", cfg.PayrollCron)
		return c
	}
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

// startChatCron は雑談 cron を開始する（Phase 4.4）。
// OFFICE_CHAT_CRON が off/空なら登録しない。
func startChatCron(
	ctx context.Context,
	cfg *config.Config,
	chat *agents.ChatAgent,
	logger *slog.Logger,
) *cron.Cron {
	c := cron.New()
	if !cfg.ChatCronEnabled() {
		logger.Info("雑談 cron は無効です", "cron", cfg.ChatCron)
		return c
	}

	_, err := c.AddFunc(cfg.ChatCron, func() {
		if !chat.Post("（雑談を一言お願いします。今のオフィスの様子でも構いません）") {
			logger.Warn("雑談 cron: chat 役の受信箱が満杯です")
		}
	})
	if err != nil {
		logger.Error("雑談 cron の登録に失敗しました", "cron", cfg.ChatCron, "error", err)
		return c
	}
	c.Start()
	logger.Info("雑談 cron を開始しました", "cron", cfg.ChatCron)
	_ = ctx
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
