package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	src := strings.NewReader(`
# comment
OFFICE_ADDR=:8787
export ANTHROPIC_API_KEY="sk-ant-abc"
OFFICE_LLM_MODEL='claude-x'
OFFICE_PAYROLL_CRON=0 9 * * *
EMPTY=
  SPACED  =  value
not a kv line
`)
	got := parseDotEnv(src)

	want := map[string]string{
		"OFFICE_ADDR":         ":8787",
		"ANTHROPIC_API_KEY":   "sk-ant-abc",
		"OFFICE_LLM_MODEL":    "claude-x",
		"OFFICE_PAYROLL_CRON": "0 9 * * *",
		"EMPTY":               "",
		"SPACED":              "value",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["not a kv line"]; ok {
		t.Error("不正な行が登録されています")
	}
}

func TestApplyDotEnvDoesNotOverride(t *testing.T) {
	t.Setenv("OFFICE_KEEP", "existing")

	applyDotEnv(map[string]string{
		"OFFICE_KEEP": "from-file",
		"OFFICE_NEW":  "from-file",
	})

	if v := os.Getenv("OFFICE_KEEP"); v != "existing" {
		t.Errorf("既存 env が上書きされました: %q", v)
	}
	if v := os.Getenv("OFFICE_NEW"); v != "from-file" {
		t.Errorf("未設定 env が反映されていません: %q", v)
	}
}
