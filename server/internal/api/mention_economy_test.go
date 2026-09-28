package api

// mention_economy_test.go は @宛先の解釈と Phase 5 の経済 API を検証する。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yukirawa/ai-office/server/internal/store"
)

func TestParseMention(t *testing.T) {
	cases := []struct {
		in     string
		target string
		body   string
	}{
		{"こんにちは", "", "こんにちは"},
		{"@mgr 点呼です", "mgr", "点呼です"},
		{"@all", "all", ""},
		{"  @chat  おはよう  ", "chat", "おはよう"},
		{"@dev_f\n作業どう？", "dev_f", "作業どう？"},
		{"@", "", ""},
	}
	for _, c := range cases {
		target, body := parseMention(c.in)
		if target != c.target || body != c.body {
			t.Errorf("parseMention(%q) = (%q, %q), want (%q, %q)", c.in, target, body, c.target, c.body)
		}
	}
}

// TestParseMentionWidthAndPunctuation は全角＠・全角スペース・句読点・コロン・
// 「さん」付きなど、IME 由来の揺れを parseMention が吸収することを確認する。
func TestParseMentionWidthAndPunctuation(t *testing.T) {
	cases := []struct {
		in     string
		target string
		body   string
	}{
		{"＠mgr　点呼", "mgr", "点呼"},
		{"@mgr、点呼", "mgr", "点呼"},
		{"@mgr：点呼", "mgr", "点呼"},
		{"@mgr: 点呼", "mgr", "点呼"},
		{"@mgr, 点呼", "mgr", "点呼"},
		{"@mgrさん、おはよう", "mgr", "さん、おはよう"},
		{"＠all　点呼", "all", "点呼"},
		{"＠dev_m　進捗どう？", "dev_m", "進捗どう？"},
		{"@", "", ""},
		{"＠", "", ""},
		{"@ だけ", "", "@ だけ"},
	}
	for _, c := range cases {
		target, body := parseMention(c.in)
		if target != c.target || body != c.body {
			t.Errorf("parseMention(%q) = (%q, %q), want (%q, %q)", c.in, target, body, c.target, c.body)
		}
	}
}

func TestLedgerEndpoints(t *testing.T) {
	rig := newTaskTestRig(t, "")
	ctx := context.Background()
	if err := rig.srv.economy.Credit(ctx, "dev_m", 42, "テスト"); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	resp, err := http.Get(rig.ts.URL + "/api/ledger")
	if err != nil {
		t.Fatalf("GET /api/ledger: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ledger status = %d", resp.StatusCode)
	}
	var all struct {
		Balances map[string]int `json:"balances"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&all)
	if all.Balances["dev_m"] != 42 {
		t.Errorf("balances = %+v, want dev_m=42", all.Balances)
	}

	resp2, err := http.Get(rig.ts.URL + "/api/ledger/dev_m/entries?limit=5")
	if err != nil {
		t.Fatalf("GET entries: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("entries status = %d", resp2.StatusCode)
	}
	var entries struct {
		Entries []struct {
			Amount int `json:"amount"`
		} `json:"entries"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&entries)
	if len(entries.Entries) != 1 || entries.Entries[0].Amount != 42 {
		t.Errorf("entries = %+v", entries.Entries)
	}
}

func TestLedgerEntriesLimitClamp(t *testing.T) {
	rig := newTaskTestRig(t, "")

	// 既定 50 件の上限を確かめるため、60 件入れる。
	entries := make([]store.LedgerEntry, 60)
	for i := range entries {
		entries[i] = store.LedgerEntry{EmployeeID: "mgr", Amount: 1, Reason: "x"}
	}
	if err := rig.st.InsertLedgerBatch(entries); err != nil {
		t.Fatalf("InsertLedgerBatch: %v", err)
	}

	cases := []struct {
		query string
		want  int
	}{
		{"", 50},            // 既定 50
		{"?limit=10", 10},   // 有効値
		{"?limit=0", 50},    // 0 は既定 50
		{"?limit=-5", 50},   // 負値は既定 50
		{"?limit=1000", 50}, // 巨大値は既定 50
		{"?limit=abc", 50},  // 非数値は既定 50
		{"?limit=200", 60},  // 上限値（60 件しか無いので全件）
	}
	for _, c := range cases {
		resp, err := http.Get(rig.ts.URL + "/api/ledger/mgr/entries" + c.query)
		if err != nil {
			t.Fatalf("GET entries%s: %v", c.query, err)
		}
		var body struct {
			Entries []json.RawMessage `json:"entries"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode%s: %v", c.query, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET entries%s status = %d, want 200", c.query, resp.StatusCode)
		}
		if len(body.Entries) != c.want {
			t.Errorf("GET entries%s = %d 件, want %d 件", c.query, len(body.Entries), c.want)
		}
	}
}

func TestLedgerUnknownEmployeeNotFound(t *testing.T) {
	rig := newTaskTestRig(t, "")

	for _, path := range []string{"/api/ledger/nobody", "/api/ledger/nobody/entries"} {
		resp, err := http.Get(rig.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestLedgerAllIncludesZeroBalanceEmployees(t *testing.T) {
	rig := newTaskTestRig(t, "")
	ctx := context.Background()
	if err := rig.srv.economy.Credit(ctx, "dev_m", 42, "テスト"); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	resp, err := http.Get(rig.ts.URL + "/api/ledger")
	if err != nil {
		t.Fatalf("GET /api/ledger: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Balances map[string]int `json:"balances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 元帳行が無い chat も残高 0 で含まれる。
	if v, ok := body.Balances["chat"]; !ok || v != 0 {
		t.Errorf("balances[chat] = %d, present=%v; want 0, true", v, ok)
	}
	if body.Balances["dev_m"] != 42 {
		t.Errorf("balances[dev_m] = %d, want 42", body.Balances["dev_m"])
	}
}

func TestEconomyStatus(t *testing.T) {
	rig := newTaskTestRig(t, "")
	ctx := context.Background()
	rig.cfg.InequalityThreshold = 50
	_ = rig.srv.economy.Credit(ctx, "mgr", 100, "x")
	_ = rig.srv.economy.Credit(ctx, "dev_m", 10, "x")

	resp, err := http.Get(rig.ts.URL + "/api/economy/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	var st economyStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 台帳に行が無い dev_f / chat（残高 0）も母集団に含むため min=0, spread=100。
	if st.Max != 100 || st.Min != 0 || st.Spread != 100 || !st.Alert {
		t.Errorf("unexpected status: %+v", st)
	}
}
