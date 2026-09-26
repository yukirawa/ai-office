package config

import (
	"testing"
	"time"
)

// clearEnv は Load が参照する環境変数をすべて空にする（=未設定扱い）。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OFFICE_ADDR", "OFFICE_DB", "OFFICE_HEARTBEAT_TIMEOUT",
		"OFFICE_PAYROLL_CRON", "OFFICE_PAYROLL_TZ",
		"OFFICE_LLM_PROVIDER", "OFFICE_LLM_MODEL", "OFFICE_ANTHROPIC_BASE_URL",
		"OFFICE_LLM_TIMEOUT", "OFFICE_LLM_MAX_TOKENS",
		"OFFICE_MAX_AGENT_TURNS", "OFFICE_TOKEN_BUDGET", "OFFICE_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Addr", cfg.Addr, ":8787"},
		{"DBPath", cfg.DBPath, "officed.db"},
		{"HeartbeatTimeout", cfg.HeartbeatTimeout, 90 * time.Second},
		{"PayrollCron", cfg.PayrollCron, "0 9 * * *"},
		{"PayrollTimezone", cfg.PayrollTimezone, "Asia/Tokyo"},
		{"LLMProvider", cfg.LLMProvider, "mock"},
		{"LLMModel", cfg.LLMModel, "claude-3-5-haiku-latest"},
		{"LLMTimeout", cfg.LLMTimeout, 120 * time.Second},
		{"LLMMaxTokens", cfg.LLMMaxTokens, 1024},
		{"AnthropicBaseURL", cfg.AnthropicBaseURL, "https://api.anthropic.com"},
		{"MaxAgentTurns", cfg.MaxAgentTurns, 8},
		{"TokenBudget", cfg.TokenBudget, 20000},
		{"LogLevel", cfg.LogLevel, "info"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("OFFICE_ADDR", ":9999")
	t.Setenv("OFFICE_DB", "/tmp/x.db")
	t.Setenv("OFFICE_HEARTBEAT_TIMEOUT", "30s")
	t.Setenv("OFFICE_MAX_AGENT_TURNS", "3")
	t.Setenv("OFFICE_TOKEN_BUDGET", "1234")
	t.Setenv("OFFICE_LLM_PROVIDER", "anthropic")
	t.Setenv("OFFICE_LLM_TIMEOUT", "5s")
	t.Setenv("OFFICE_LLM_MAX_TOKENS", "256")
	t.Setenv("OFFICE_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" || cfg.DBPath != "/tmp/x.db" {
		t.Errorf("Addr/DBPath = %q/%q", cfg.Addr, cfg.DBPath)
	}
	if cfg.HeartbeatTimeout != 30*time.Second {
		t.Errorf("HeartbeatTimeout = %v, want 30s", cfg.HeartbeatTimeout)
	}
	if cfg.MaxAgentTurns != 3 || cfg.TokenBudget != 1234 {
		t.Errorf("MaxAgentTurns/TokenBudget = %d/%d", cfg.MaxAgentTurns, cfg.TokenBudget)
	}
	if cfg.LLMProvider != "anthropic" || cfg.LogLevel != "debug" {
		t.Errorf("LLMProvider/LogLevel = %q/%q", cfg.LLMProvider, cfg.LogLevel)
	}
	if cfg.LLMTimeout != 5*time.Second || cfg.LLMMaxTokens != 256 {
		t.Errorf("LLMTimeout/LLMMaxTokens = %v/%d", cfg.LLMTimeout, cfg.LLMMaxTokens)
	}
}

func TestLoadInvalidEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("OFFICE_HEARTBEAT_TIMEOUT", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Error("不正な duration でエラーにならなかった")
	}

	clearEnv(t)
	t.Setenv("OFFICE_MAX_AGENT_TURNS", "abc")
	if _, err := Load(); err == nil {
		t.Error("不正な整数でエラーにならなかった")
	}

	clearEnv(t)
	t.Setenv("OFFICE_LLM_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Error("不正な OFFICE_LLM_TIMEOUT でエラーにならなかった")
	}

	clearEnv(t)
	t.Setenv("OFFICE_LLM_MAX_TOKENS", "lots")
	if _, err := Load(); err == nil {
		t.Error("不正な OFFICE_LLM_MAX_TOKENS でエラーにならなかった")
	}
}

func TestDefaultEmployees(t *testing.T) {
	seeds := DefaultEmployees()
	if len(seeds) != 4 {
		t.Fatalf("DefaultEmployees = %d 件, want 4 件", len(seeds))
	}
	want := map[string]struct {
		role     string
		deviceID string
	}{
		EmployeeManagerID: {RoleManager, "server"},
		EmployeeDevMID:    {RoleWorker, "zenbook"},
		EmployeeDevFID:    {RoleWorker, "zenbook"},
		EmployeeChatID:    {RoleChat, "zenbook"},
	}
	for _, seed := range seeds {
		w, ok := want[seed.ID]
		if !ok {
			t.Errorf("未知の社員 ID: %q", seed.ID)
			continue
		}
		if seed.Name == "" {
			t.Errorf("%s の Name が空", seed.ID)
		}
		if seed.Role != w.role || seed.DeviceID != w.deviceID {
			t.Errorf("%s = role %q device %q, want role %q device %q",
				seed.ID, seed.Role, seed.DeviceID, w.role, w.deviceID)
		}
		if seed.PersonaJSON == "" || seed.PersonaJSON[0] != '{' {
			t.Errorf("%s の PersonaJSON が JSON でない: %q", seed.ID, seed.PersonaJSON)
		}
	}
}

func TestSalaries(t *testing.T) {
	if got := BaseSalaryByRole(RoleManager); got != 500 {
		t.Errorf("BaseSalaryByRole(manager) = %d, want 500", got)
	}
	if got := BaseSalaryByRole(RoleWorker); got != 350 {
		t.Errorf("BaseSalaryByRole(worker) = %d, want 350", got)
	}
	if got := BaseSalaryByRole(RoleChat); got != 0 {
		t.Errorf("BaseSalaryByRole(chat) = %d, want 0", got)
	}
	if got := BaseSalaryByRole("unknown"); got != 0 {
		t.Errorf("BaseSalaryByRole(unknown) = %d, want 0", got)
	}

	if got := MonthlySalary(EmployeeManagerID); got != 500 {
		t.Errorf("MonthlySalary(mgr) = %d, want 500", got)
	}
	if got := MonthlySalary(EmployeeDevMID); got != 350 {
		t.Errorf("MonthlySalary(dev_m) = %d, want 350", got)
	}
	if got := MonthlySalary("unknown"); got != 0 {
		t.Errorf("MonthlySalary(unknown) = %d, want 0", got)
	}

	if got := DailyWage(EmployeeManagerID); got != 500/PayrollDivisor {
		t.Errorf("DailyWage(mgr) = %d, want %d", got, 500/PayrollDivisor)
	}
	if got := DailyWage(EmployeeDevMID); got != 350/PayrollDivisor {
		t.Errorf("DailyWage(dev_m) = %d, want %d", got, 350/PayrollDivisor)
	}
	if got := DailyWage(EmployeeChatID); got != 0 {
		t.Errorf("DailyWage(chat) = %d, want 0", got)
	}
}
