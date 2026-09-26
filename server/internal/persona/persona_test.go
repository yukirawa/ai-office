package persona

import "testing"

func TestLoadEmpty(t *testing.T) {
	p := Load("")
	if p.Name != "" || p.SystemPrompt() != "" {
		t.Fatalf("expected zero persona, got %+v", p)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	p := Load("{not json")
	if p.Name != "" {
		t.Fatalf("expected zero persona on invalid json, got %+v", p)
	}
}

func TestLoadAndSystemPrompt(t *testing.T) {
	p := Load(`{"name":"マネージャー","role":"窓口","tone":"丁寧","traits":{"careful":"high"}}`)
	got := p.SystemPrompt()
	if got == "" {
		t.Fatal("expected non-empty system prompt")
	}
	for _, want := range []string{"マネージャー", "窓口", "丁寧", "careful=high"} {
		if !contains(got, want) {
			t.Errorf("system prompt %q missing %q", got, want)
		}
	}
}

func TestSystemOverride(t *testing.T) {
	p := Load(`{"name":"x","system":"完全固定プロンプト"}`)
	if p.SystemPrompt() != "完全固定プロンプト" {
		t.Fatalf("expected override, got %q", p.SystemPrompt())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
