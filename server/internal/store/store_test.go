package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/config"
)

// openTestStore は temp ディレクトリの下に SQLite を作る。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("1 回目の Open: %v", err)
	}
	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		t.Fatalf("SeedEmployees: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2 回目の Open でマイグレーションが再適用されても壊れないこと。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("2 回目の Open: %v", err)
	}
	defer st2.Close()

	emps, err := st2.Employees()
	if err != nil {
		t.Fatalf("Employees: %v", err)
	}
	if len(emps) != len(config.DefaultEmployees()) {
		t.Fatalf("Employees = %d 件, want %d 件", len(emps), len(config.DefaultEmployees()))
	}
}

func TestSeedAndEmployees(t *testing.T) {
	st := openTestStore(t)

	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		t.Fatalf("SeedEmployees: %v", err)
	}
	// 冪等（2 回目で重複しない）。
	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		t.Fatalf("SeedEmployees(2): %v", err)
	}

	emps, err := st.Employees()
	if err != nil {
		t.Fatalf("Employees: %v", err)
	}
	wantIDs := []string{"chat", "dev_f", "dev_m", "mgr"} // ID 昇順
	if len(emps) != len(wantIDs) {
		t.Fatalf("Employees = %d 件, want %d 件", len(emps), len(wantIDs))
	}
	for i, want := range wantIDs {
		if emps[i].ID != want {
			t.Errorf("emps[%d].ID = %q, want %q", i, emps[i].ID, want)
		}
	}

	mgr, err := st.Employee("mgr")
	if err != nil {
		t.Fatalf("Employee(mgr): %v", err)
	}
	if mgr.Name == "" || mgr.Role != config.RoleManager {
		t.Errorf("mgr = %+v, want name 非空 / role manager", mgr)
	}

	if _, err := st.Employee("nobody"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Employee(nobody) err = %v, want sql.ErrNoRows", err)
	}
}

func TestUpsertEmployeePreservesCreatedAt(t *testing.T) {
	st := openTestStore(t)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.UpsertEmployee(Employee{ID: "dev_m", Name: "旧名", Role: config.RoleWorker, CreatedAt: created}); err != nil {
		t.Fatalf("UpsertEmployee: %v", err)
	}
	if err := st.UpsertEmployee(Employee{ID: "dev_m", Name: "新名", Role: config.RoleWorker}); err != nil {
		t.Fatalf("UpsertEmployee(2): %v", err)
	}

	e, err := st.Employee("dev_m")
	if err != nil {
		t.Fatalf("Employee: %v", err)
	}
	if e.Name != "新名" {
		t.Errorf("Name = %q, want 新名", e.Name)
	}
	if !e.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v（更新で上書きされない）", e.CreatedAt, created)
	}
}

func TestSessions(t *testing.T) {
	st := openTestStore(t)

	if err := st.StartSession("s1", "dev_m", "zenbook"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	active, err := st.ActiveSession("dev_m")
	if err != nil {
		t.Fatalf("ActiveSession: %v", err)
	}
	if active.ID != "s1" || active.EndedAt != nil {
		t.Fatalf("active = %+v, want id=s1 ended=nil", active)
	}

	if err := st.EndSession("s1", "shutdown"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, err := st.ActiveSession("dev_m"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("ActiveSession after end err = %v, want sql.ErrNoRows", err)
	}

	// EndActiveSessions は未終了のみ閉じる。
	if err := st.StartSession("s2", "dev_m", "zenbook"); err != nil {
		t.Fatalf("StartSession s2: %v", err)
	}
	if err := st.EndActiveSessions("dev_m", "timeout"); err != nil {
		t.Fatalf("EndActiveSessions: %v", err)
	}
	if _, err := st.ActiveSession("dev_m"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("ActiveSession after EndActiveSessions err = %v, want sql.ErrNoRows", err)
	}
}

func TestMessagesChronologicalAndLimit(t *testing.T) {
	st := openTestStore(t)
	for _, c := range []string{"#会議室", "#会議室", "#雑談"} {
		if _, err := st.InsertMessage(Message{Channel: c, FromID: "mgr", Content: "hello"}); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
	}

	msgs, err := st.Messages("#会議室", 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Messages(#会議室) = %d 件, want 2 件", len(msgs))
	}
	if msgs[0].ID >= msgs[1].ID {
		t.Errorf("古い順になっていない: %d, %d", msgs[0].ID, msgs[1].ID)
	}

	// limit は「直近 N 件」を古い順で返す。
	limited, err := st.Messages("#会議室", 1)
	if err != nil {
		t.Fatalf("Messages(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != msgs[1].ID {
		t.Fatalf("Messages(limit=1) = %+v, want 直近の %d", limited, msgs[1].ID)
	}

	// 全チャンネル。
	all, err := st.Messages("", 0)
	if err != nil {
		t.Fatalf("Messages(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("Messages(all) = %d 件, want 3 件", len(all))
	}
}

func TestLedgerAndBalance(t *testing.T) {
	st := openTestStore(t)
	entries := []LedgerEntry{
		{EmployeeID: "dev_m", Amount: 500, Reason: "月給"},
		{EmployeeID: "dev_m", Amount: -120, Reason: "購入"},
		{EmployeeID: "dev_f", Amount: 350, Reason: "月給"},
	}
	for _, e := range entries {
		if err := st.InsertLedger(e); err != nil {
			t.Fatalf("InsertLedger: %v", err)
		}
	}

	if got, err := st.Balance("dev_m"); err != nil || got != 380 {
		t.Fatalf("Balance(dev_m) = %d, %v; want 380", got, err)
	}
	if got, err := st.Balance("chat"); err != nil || got != 0 {
		t.Fatalf("Balance(chat) = %d, %v; want 0", got, err)
	}

	log, err := st.Ledger("dev_m", 0)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(log) != 2 || log[0].ID >= log[1].ID {
		t.Fatalf("Ledger が古い順でない: %+v", log)
	}

	balances, err := st.AllBalances()
	if err != nil {
		t.Fatalf("AllBalances: %v", err)
	}
	if balances["dev_m"] != 380 || balances["dev_f"] != 350 {
		t.Fatalf("AllBalances = %v", balances)
	}
}

func TestInsertLedgerBatch(t *testing.T) {
	st := openTestStore(t)
	if err := st.InsertLedgerBatch([]LedgerEntry{
		{EmployeeID: "mgr", Amount: 16, Reason: "日割り"},
		{EmployeeID: "dev_m", Amount: 11, Reason: "日割り"},
	}); err != nil {
		t.Fatalf("InsertLedgerBatch: %v", err)
	}
	if got, _ := st.Balance("mgr"); got != 16 {
		t.Errorf("Balance(mgr) = %d, want 16", got)
	}
	if err := st.InsertLedgerBatch(nil); err != nil {
		t.Errorf("InsertLedgerBatch(nil) = %v, want nil", err)
	}
}

func TestRelationships(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertRelationship(Relationship{FromID: "mgr", ToID: "dev_m", Affinity: 10, Trust: 50}); err != nil {
		t.Fatalf("UpsertRelationship: %v", err)
	}
	// 同じ組を upsert すると更新される。
	if err := st.UpsertRelationship(Relationship{FromID: "mgr", ToID: "dev_m", Affinity: 15, Trust: 60}); err != nil {
		t.Fatalf("UpsertRelationship(2): %v", err)
	}
	if err := st.UpsertRelationship(Relationship{FromID: "mgr", ToID: "dev_f", Affinity: -5, Trust: 20}); err != nil {
		t.Fatalf("UpsertRelationship(3): %v", err)
	}

	rels, err := st.Relationships("mgr")
	if err != nil {
		t.Fatalf("Relationships: %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("Relationships(mgr) = %d 件, want 2 件", len(rels))
	}
	if rels[0].ToID != "dev_f" || rels[1].ToID != "dev_m" {
		t.Fatalf("to_id 昇順でない: %+v", rels)
	}
	if rels[1].Affinity != 15 || rels[1].Trust != 60 {
		t.Errorf("upsert で更新されていない: %+v", rels[1])
	}

	all, err := st.AllRelationships()
	if err != nil {
		t.Fatalf("AllRelationships: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllRelationships = %d 件, want 2 件", len(all))
	}
}

func TestTasks(t *testing.T) {
	st := openTestStore(t)
	t0 := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	tasks := []Task{
		{ID: "t1", Title: "古いタスク", Status: "pending", CreatedAt: t0},
		{ID: "t2", Title: "新しいタスク", Status: "working", CreatedAt: t0.Add(time.Hour)},
	}
	for _, task := range tasks {
		if err := st.InsertTask(task); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
	}

	got, err := st.Task("t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got.Title != "古いタスク" || got.Status != "pending" {
		t.Errorf("Task(t1) = %+v", got)
	}

	// 新しい順。
	list, err := st.Tasks("", 0)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(list) != 2 || list[0].ID != "t2" {
		t.Fatalf("Tasks = %+v, want 新しい順 (t2 が先頭)", list)
	}

	// ステータス絞り込み。
	working, err := st.Tasks("working", 0)
	if err != nil {
		t.Fatalf("Tasks(working): %v", err)
	}
	if len(working) != 1 || working[0].ID != "t2" {
		t.Fatalf("Tasks(working) = %+v", working)
	}

	if err := st.UpdateTask("t1", "done", "完了しました"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	updated, err := st.Task("t1")
	if err != nil {
		t.Fatalf("Task after update: %v", err)
	}
	if updated.Status != "done" || updated.Result != "完了しました" {
		t.Errorf("UpdateTask が反映されていない: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Errorf("UpdatedAt = %v, want CreatedAt(%v) より後", updated.UpdatedAt, updated.CreatedAt)
	}

	if err := st.UpdateTask("nobody", "done", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateTask(nobody) err = %v, want sql.ErrNoRows", err)
	}
}
