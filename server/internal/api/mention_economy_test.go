package api

// mention_economy_test.go は @宛先の解釈と Phase 5 の経済 API を検証する。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/yukirawa/ai-office/server/internal/agents"
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

// fakeAgent は agents.Agent のテスト用実装。
type fakeAgent struct {
	mu    sync.Mutex
	model string
}

func (f *fakeAgent) Converse(context.Context, string, string) {}
func (f *fakeAgent) SetModel(model string) {
	f.mu.Lock()
	f.model = model
	f.mu.Unlock()
}
func (f *fakeAgent) Model() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.model
}

func TestPurchaseDebitsAndSetsModel(t *testing.T) {
	rig := newTaskTestRig(t, "")
	rig.cfg.PremiumModel = "premium-x"
	rig.cfg.PremiumModelPrice = 10

	fa := &fakeAgent{}
	rig.srv.SetAgents(map[string]agents.Agent{"mgr": fa})

	if err := rig.srv.economy.Credit(context.Background(), "mgr", 1000, "test"); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	resp, err := http.Post(rig.ts.URL+"/api/economy/purchase", "application/json",
		strings.NewReader(`{"employee_id":"mgr"}`))
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Model   string `json:"model"`
		Price   int    `json:"price"`
		Balance int    `json:"balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Model != "premium-x" || body.Price != 10 || body.Balance != 990 {
		t.Errorf("unexpected body: %+v", body)
	}
	if got := fa.Model(); got != "premium-x" {
		t.Errorf("agent model = %q, want premium-x", got)
	}
}

func TestPurchaseInsufficientBalance(t *testing.T) {
	rig := newTaskTestRig(t, "")
	rig.cfg.PremiumModelPrice = 100
	rig.srv.SetAgents(map[string]agents.Agent{"mgr": &fakeAgent{}})

	resp, err := http.Post(rig.ts.URL+"/api/economy/purchase", "application/json",
		strings.NewReader(`{"employee_id":"mgr"}`))
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
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
	if st.Max != 100 || st.Min != 10 || st.Spread != 90 || !st.Alert {
		t.Errorf("unexpected status: %+v", st)
	}
}
