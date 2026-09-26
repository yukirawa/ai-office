package api

// tasks_test.go は Phase 2/3 のタスク実行を api 側から検証する。
//   - WS 接続した偽 worker が task_assign に task_result を返し、タスクが done になる
//   - worker 不在時は Dispatch がエラーになる
//   - RemoteExecutor が gh.Client 経由で PR を作る
//   - GitHub webhook がタスク化される（署名検証込み）

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/economy"
	"github.com/yukirawa/ai-office/server/internal/gh"
	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
	"github.com/yukirawa/ai-office/server/internal/presence"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// taskTestRig は manager / dev エージェントまで配線したテスト環境。
type taskTestRig struct {
	srv *Server
	st  *store.Store
	reg *presence.Registry
	ts  *httptest.Server
	cfg *config.Config
}

func newTaskTestRig(t *testing.T, webhookSecret string) *taskTestRig {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SeedEmployees(config.DefaultEmployees()); err != nil {
		t.Fatalf("SeedEmployees: %v", err)
	}

	cfg := &config.Config{
		HeartbeatTimeout:    90 * time.Second,
		GitHubWebhookSecret: webhookSecret,
		GitHubBaseBranch:    "main",
	}
	reg := presence.NewRegistry()
	econ := economy.New(st)

	srv := New(Deps{Cfg: cfg, Store: st, Presence: reg, Economy: econ})

	// モック LLM は散文を返すので、dev は決定的フォールバック（write reports/<id>.md）を使う。
	client := llm.NewMock()
	opts := agents.Options{Channel: "#会議室"}

	mgr := agents.NewManager("mgr", persona.Load(`{"name":"ミカ"}`), client, srv, opts)
	devM := agents.NewDevAgent("dev_m", persona.Load(`{"name":"タクミ"}`), client, srv, srv, srv, srv, mgr, opts)
	devF := agents.NewDevAgent("dev_f", persona.Load(`{"name":"アナ"}`), client, srv, srv, srv, srv, mgr, opts)
	mgr.SetAssignees(devM, devF)
	mgr.SetTaskUpdater(srv)
	srv.SetManager(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go mgr.Run(ctx)
	go devM.Run(ctx)
	go devF.Run(ctx)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &taskTestRig{srv: srv, st: st, reg: reg, ts: ts, cfg: cfg}
}

// fakeWorker は dev_m として接続し、task_assign に task_result を返す偽 worker。
// 受信した task_id を assigned チャネルへ流す。
func startFakeWorker(t *testing.T, ctx context.Context, url string, assigned chan<- string) {
	t.Helper()

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"hello","employee_id":"dev_m","device_id":"zenbook","version":"0.1.0"}`)); err != nil {
		t.Fatalf("worker hello: %v", err)
	}

	go func() {
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env struct {
				Type   string `json:"type"`
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type != "task_assign" {
				continue
			}
			select {
			case assigned <- env.TaskID:
			default:
			}
			result := fmt.Sprintf(
				`{"type":"task_result","task_id":%q,"status":"done","summary":"偽 worker が実行しました","detail":"","artifacts":[]}`,
				env.TaskID)
			_ = conn.Write(ctx, websocket.MessageText, []byte(result))
		}
	}()
}

func TestTaskFlowWithFakeWorker(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	assigned := make(chan string, 4)
	startFakeWorker(t, ctx, wsURL(rig.ts.URL), assigned)

	// worker が在席になるまで待つ。
	waitFor(t, ctx, func() bool {
		_, ok := rig.reg.Get("dev_m")
		return ok
	}, "dev_m が在席になりません")

	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{Title: "結合テスト", From: "owner"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	select {
	case gotID := <-assigned:
		if gotID != id {
			t.Errorf("task_assign の task_id = %q, want %q", gotID, id)
		}
	case <-ctx.Done():
		t.Fatal("task_assign が届きませんでした")
	}

	// タスクが done になるまで待つ。
	waitFor(t, ctx, func() bool {
		task, err := rig.st.Task(id)
		return err == nil && task.Status == "done"
	}, "タスクが done になりません")
}

func TestDispatchFailsWhenWorkerOffline(t *testing.T) {
	rig := newTaskTestRig(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rig.srv.Dispatch(ctx, "dev_f", agents.TaskAssign{TaskID: "t1", Mode: "local", Title: "x"})
	if err == nil {
		t.Fatal("worker 不在なら Dispatch はエラーになるべきです")
	}
	if !strings.Contains(err.Error(), "接続していません") {
		t.Errorf("エラーメッセージが想定外: %v", err)
	}
}

// fakeGitHub は gh.Client の最小スタブ。
type fakeGitHub struct {
	prCalls int
}

func (f *fakeGitHub) InstallationToken(context.Context) (string, error) { return "tok", nil }
func (f *fakeGitHub) CreatePullRequest(_ context.Context, req gh.PRRequest) (gh.PRResult, error) {
	f.prCalls++
	return gh.PRResult{Number: 42, HTMLURL: "https://example.test/pr/42", Branch: req.Branch}, nil
}

func TestExecuteRemoteCreatesPR(t *testing.T) {
	rig := newTaskTestRig(t, "")
	fake := &fakeGitHub{}
	rig.srv.SetGitHub(fake)

	res, err := rig.srv.ExecuteRemote(context.Background(), agents.RemoteSpec{
		Repo:   "owner/name",
		Branch: "ai-office/task-1",
		Title:  "テスト",
		Files:  []agents.RemoteFile{{Path: "a.txt", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ExecuteRemote: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("status = %q, want done", res.Status)
	}
	if !strings.Contains(res.Summary, "https://example.test/pr/42") {
		t.Errorf("summary に PR URL がありません: %q", res.Summary)
	}
	if fake.prCalls != 1 {
		t.Errorf("prCalls = %d, want 1", fake.prCalls)
	}
}

// TestRemoteTaskFallsBackToServer は「PC オフライン時はサーバーが GitHub 上で完結する」（§2）
// 経路を検証する。worker を接続させず、remote タスクを投入すると、dev の Dispatch が
// 失敗し、api の RemoteExecutor（gh クライアント）経由で PR 作成まで進む。
func TestRemoteTaskFallsBackToServer(t *testing.T) {
	rig := newTaskTestRig(t, "")
	fake := &fakeGitHub{}
	rig.srv.SetGitHub(fake)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	id, err := rig.srv.CreateTask(ctx, CreateTaskInput{
		Title:      "remote タスク",
		From:       "owner",
		Mode:       "remote",
		Repo:       "owner/name",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	waitFor(t, ctx, func() bool {
		task, err := rig.st.Task(id)
		return err == nil && task.Status == "done" && strings.Contains(task.Result, "https://example.test/pr/42")
	}, "remote タスクがサーバー側で done になりません")

	if fake.prCalls == 0 {
		t.Error("サーバー側の PR 作成が呼ばれていません")
	}
}

func TestGitHubWebhookCreatesTask(t *testing.T) {
	const secret = "test-secret"
	rig := newTaskTestRig(t, secret)

	body := []byte(`{
		"action":"opened",
		"repository":{"full_name":"owner/name"},
		"issue":{"number":7,"title":"ログインが壊れている","body":"再現手順..."},
		"sender":{"login":"octocat"}
	}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest(http.MethodPost, rig.ts.URL+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// remote モードのタスクが作られているはず。
	waitFor(t, context.Background(), func() bool {
		tasks, err := rig.st.Tasks("", 10)
		if err != nil {
			return false
		}
		for _, task := range tasks {
			if task.Mode == "remote" && task.Repo == "owner/name" && strings.Contains(task.Title, "#7") {
				return true
			}
		}
		return false
	}, "webhook から remote タスクが作られません")
}

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	rig := newTaskTestRig(t, "test-secret")

	req, _ := http.NewRequest(http.MethodPost, rig.ts.URL+"/webhook/github",
		strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Event", "issues")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// waitFor は条件が満たされるまで短い間隔で待つ。
func waitFor(t *testing.T, ctx context.Context, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
