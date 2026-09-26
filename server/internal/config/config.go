// Package config は環境変数からサーバ設定と初期社員データを提供する。
//
// 設計書 §6.1 の config パッケージに対応する。設定値はここに集約し、
// 他パッケージにハードコードしない（§11 の「設定値は定数 or 環境変数に」の指針）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 既定値。すべて環境変数で上書き可能。
const (
	defaultAddr             = ":8787"
	defaultDBPath           = "officed.db"
	defaultHeartbeatTimeout = 90 * time.Second
	defaultPayrollCron      = "0 9 * * *"
	defaultPayrollTimezone  = "Asia/Tokyo"
	defaultChatCron         = "0 * * * *"
	defaultLLMProvider      = "mock"
	defaultLLMModel         = "claude-3-5-haiku-latest"
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultMaxAgentTurns    = 8
	defaultTokenBudget      = 20000
	defaultLogLevel         = "info"
)

// 社員 ID。設計書 §3 の組織に対応する。
// 他のパッケージからはこの定数を使い、文字列リテラルを散らさない。
const (
	EmployeeManagerID = "mgr"
	EmployeeDevMID    = "dev_m"
	EmployeeDevFID    = "dev_f"
	EmployeeChatID    = "chat"
)

// employees.role の enum（設計書 §5）。
const (
	RoleManager = "manager"
	RoleWorker  = "worker"
	RoleChat    = "chat"
)

// PayrollDivisor は月給を日割りする際の除数。
// 設計書 §3 の chat（日当制）および Phase 1.4 の日割り給与 cron で使う。
const PayrollDivisor = 30

// Config はサーバ全体の設定。
type Config struct {
	// Addr は HTTP の待ち受けアドレス（env OFFICE_ADDR）。
	Addr string
	// DBPath は SQLite ファイルのパス（env OFFICE_DB）。
	DBPath string
	// HeartbeatTimeout は無応答を退勤扱いにするまでの時間（env OFFICE_HEARTBEAT_TIMEOUT）。
	HeartbeatTimeout time.Duration
	// PayrollCron は日割り給与の cron 式（env OFFICE_PAYROLL_CRON）。
	PayrollCron string
	// PayrollTimezone は cron のタイムゾーン（env OFFICE_PAYROLL_TZ）。
	PayrollTimezone string
	// ChatCron は雑談 cron 式（env OFFICE_CHAT_CRON）。"off" で無効（§8 4.4）。
	ChatCron string
	// LLMProvider は "mock" または "anthropic"（env OFFICE_LLM_PROVIDER）。
	LLMProvider string
	// LLMModel は LLM のモデル名（env OFFICE_LLM_MODEL）。
	LLMModel string
	// AnthropicAPIKey は Anthropic API キー（env ANTHROPIC_API_KEY、無ければ secrets/anthropic.key）。
	AnthropicAPIKey string
	// AnthropicBaseURL は Anthropic API のベース URL（env OFFICE_ANTHROPIC_BASE_URL）。
	AnthropicBaseURL string
	// MaxAgentTurns はエージェント 1 タスクあたりの最大ターン数（env OFFICE_MAX_AGENT_TURNS）。
	MaxAgentTurns int
	// TokenBudget はエージェント 1 タスクあたりのトークン予算（env OFFICE_TOKEN_BUDGET）。
	TokenBudget int
	// LogLevel は slog のレベル名（env OFFICE_LOG_LEVEL）。
	LogLevel string

	// ---- Phase 3: GitHub ----

	// GitHubAppID は GitHub App の ID（env GITHUB_APP_ID）。0 なら GitHub 連携は無効。
	GitHubAppID int64
	// GitHubInstallationID はインストール ID（env GITHUB_INSTALLATION_ID）。
	GitHubInstallationID int64
	// GitHubPrivateKeyPath は App の秘密鍵 PEM のパス（env GITHUB_APP_PRIVATE_KEY_PATH）。
	GitHubPrivateKeyPath string
	// GitHubPrivateKeyPEM は秘密鍵の内容（env GITHUB_PRIVATE_KEY があれば優先、無ければパスから読む）。
	GitHubPrivateKeyPEM string
	// GitHubWebhookSecret は webhook 署名検証用の secret（env GITHUB_WEBHOOK_SECRET）。
	GitHubWebhookSecret string
	// GitHubRepo は対象リポジトリ（env GITHUB_REPO、"owner/name"）。
	GitHubRepo string
	// GitHubBaseBranch は PR のベースブランチ（env GITHUB_BASE_BRANCH）。
	GitHubBaseBranch string
}

// Load は環境変数から Config を読み込む。
// 空文字の環境変数は「未設定」として既定値を使う。
func Load() (*Config, error) {
	heartbeat, err := durationEnv("OFFICE_HEARTBEAT_TIMEOUT", defaultHeartbeatTimeout)
	if err != nil {
		return nil, err
	}
	maxTurns, err := intEnv("OFFICE_MAX_AGENT_TURNS", defaultMaxAgentTurns)
	if err != nil {
		return nil, err
	}
	tokenBudget, err := intEnv("OFFICE_TOKEN_BUDGET", defaultTokenBudget)
	if err != nil {
		return nil, err
	}
	ghAppID, err := int64Env("GITHUB_APP_ID", 0)
	if err != nil {
		return nil, err
	}
	ghInstallationID, err := int64Env("GITHUB_INSTALLATION_ID", 0)
	if err != nil {
		return nil, err
	}

	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		// 設計書 §9: 秘密情報は secrets/ に置く。無くても起動は止めない
		// （mock プロバイダならキー不要なので）。
		apiKey = readSecretFile("anthropic.key")
	}

	ghKeyPath := stringEnv("GITHUB_APP_PRIVATE_KEY_PATH", filepath.Join("secrets", "github-app.pem"))
	ghPEM := strings.TrimSpace(os.Getenv("GITHUB_PRIVATE_KEY"))
	if ghPEM == "" {
		ghPEM = readSecretPath(ghKeyPath)
	}

	return &Config{
		Addr:                 stringEnv("OFFICE_ADDR", defaultAddr),
		DBPath:               stringEnv("OFFICE_DB", defaultDBPath),
		HeartbeatTimeout:     heartbeat,
		PayrollCron:          stringEnv("OFFICE_PAYROLL_CRON", defaultPayrollCron),
		PayrollTimezone:      stringEnv("OFFICE_PAYROLL_TZ", defaultPayrollTimezone),
		ChatCron:             stringEnv("OFFICE_CHAT_CRON", defaultChatCron),
		LLMProvider:          stringEnv("OFFICE_LLM_PROVIDER", defaultLLMProvider),
		LLMModel:             stringEnv("OFFICE_LLM_MODEL", defaultLLMModel),
		AnthropicAPIKey:      apiKey,
		AnthropicBaseURL:     stringEnv("OFFICE_ANTHROPIC_BASE_URL", defaultAnthropicBaseURL),
		MaxAgentTurns:        maxTurns,
		TokenBudget:          tokenBudget,
		LogLevel:             stringEnv("OFFICE_LOG_LEVEL", defaultLogLevel),
		GitHubAppID:          ghAppID,
		GitHubInstallationID: ghInstallationID,
		GitHubPrivateKeyPath: ghKeyPath,
		GitHubPrivateKeyPEM:  ghPEM,
		GitHubWebhookSecret:  os.Getenv("GITHUB_WEBHOOK_SECRET"),
		GitHubRepo:           strings.TrimSpace(os.Getenv("GITHUB_REPO")),
		GitHubBaseBranch:     stringEnv("GITHUB_BASE_BRANCH", "main"),
	}, nil
}

// GitHubEnabled は GitHub App 連携に必要な設定が揃っているかを返す。
func (c *Config) GitHubEnabled() bool {
	return c.GitHubAppID > 0 && c.GitHubInstallationID > 0 && strings.TrimSpace(c.GitHubPrivateKeyPEM) != ""
}

// ChatCronEnabled は雑談 cron が有効かを返す。空文字・"off"/"none"/"disabled" は無効。
func (c *Config) ChatCronEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(c.ChatCron)) {
	case "", "off", "none", "disabled":
		return false
	default:
		return true
	}
}

// stringEnv は環境変数を読み、空文字なら既定値を返す。
func stringEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// intEnv は整数の環境変数を読む。空文字・未設定なら既定値。
func intEnv(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s は整数ではありません: %q: %w", key, v, err)
	}
	return n, nil
}

// int64Env は 64bit 整数の環境変数を読む。空文字・未設定なら既定値。
func int64Env(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s は整数ではありません: %q: %w", key, v, err)
	}
	return n, nil
}

// durationEnv は Go duration 形式の環境変数を読む。空文字・未設定なら既定値。
func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s は duration ではありません: %q: %w", key, v, err)
	}
	return d, nil
}

// secretDirs は secrets ディレクトリを探す候補。
// サーバをリポジトリ直下から起動した場合と server/ から起動した場合の両方に対応する。
var secretDirs = []string{
	"secrets",
	filepath.Join("..", "secrets"),
}

// readSecretFile は secrets/<name> を読み、前後の空白を除いた内容を返す。
// 見つからなければ空文字を返す（エラーにはしない）。
func readSecretFile(name string) string {
	for _, dir := range secretDirs {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	return ""
}

// readSecretPath は指定パスのファイルを読み、前後の空白を除いた内容を返す。
// 見つからない場合は secrets/<basename> も探す。
// どちらも無ければ空文字を返す（エラーにはしない）。
func readSecretPath(path string) string {
	if data, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v
		}
	}
	return readSecretFile(filepath.Base(path))
}

// EmployeeSeed は初期社員の定義。store.SeedEmployees に渡して upsert する。
type EmployeeSeed struct {
	ID          string
	Name        string
	Role        string
	Gender      string
	DeviceID    string
	PersonaJSON string
}

// DefaultEmployees は設計書 §3 の初期社員 4 名を返す。
// 名前は短い日本語表示名、PersonaJSON は persona パッケージが読む JSON。
func DefaultEmployees() []EmployeeSeed {
	return []EmployeeSeed{
		{
			ID:       EmployeeManagerID,
			Name:     "ミカ",
			Role:     RoleManager,
			Gender:   "男",
			DeviceID: "server",
			PersonaJSON: personaJSON("ミカ", "管理職・オーナーの窓口", "男",
				"丁寧だが親しみやすい"),
		},
		{
			ID:       EmployeeDevMID,
			Name:     "タクミ",
			Role:     RoleWorker,
			Gender:   "男",
			DeviceID: "zenbook",
			PersonaJSON: personaJSON("タクミ", "平社員（開発・レビュー）", "男",
				"ぶっきらぼうだが誠実"),
		},
		{
			ID:       EmployeeDevFID,
			Name:     "アヤナ",
			Role:     RoleWorker,
			Gender:   "女",
			DeviceID: "zenbook",
			PersonaJSON: personaJSON("アヤナ", "平社員（開発・レビュー）", "女",
				"明るく丁寧"),
		},
		{
			ID:       EmployeeChatID,
			Name:     "チャット",
			Role:     RoleChat,
			Gender:   "未定",
			DeviceID: "zenbook",
			PersonaJSON: personaJSON("チャット", "雑談・雑用（日雇い）", "未定",
				"気さくでゆるい"),
		},
	}
}

// personaJSON は persona_json 用の小さな JSON を組み立てる。
// 入力は固定値なので marshal 失敗時は空オブジェクトにフォールバックする。
func personaJSON(name, role, gender, tone string) string {
	v := struct {
		Name   string `json:"name"`
		Role   string `json:"role"`
		Gender string `json:"gender"`
		Tone   string `json:"tone"`
	}{Name: name, Role: role, Gender: gender, Tone: tone}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// baseSalaryByRole は役割ごとの月給（学/月）。設計書 §3。
var baseSalaryByRole = map[string]int{
	RoleManager: 500,
	RoleWorker:  350,
	RoleChat:    0, // 日当制（Phase 4）
}

// monthlySalaryByID は社員 ID ごとの月給（学/月）。設計書 §3。
var monthlySalaryByID = map[string]int{
	EmployeeManagerID: 500,
	EmployeeDevMID:    350,
	EmployeeDevFID:    350,
	EmployeeChatID:    0, // 日当制（Phase 4）
}

// BaseSalaryByRole は役割ごとの月給を返す。未知の役割は 0。
func BaseSalaryByRole(role string) int {
	return baseSalaryByRole[role]
}

// MonthlySalary は社員 ID の月給を返す。未知の ID は 0
// （ID から役割は引けないため、BaseSalaryByRole へのフォールバックはしない）。
func MonthlySalary(employeeID string) int {
	return monthlySalaryByID[employeeID]
}

// DailyWage は月給の日割り額（月給 / PayrollDivisor）を返す。
// Phase 1.4 の日割り給与 cron が使う。
func DailyWage(employeeID string) int {
	return MonthlySalary(employeeID) / PayrollDivisor
}
