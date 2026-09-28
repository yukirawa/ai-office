// DevAgent は Phase 2.4 の dev エージェント。mgr が立てた計画を受け取り、
// worker（local / remote）に割り当てて結果を待ち、mgr にレビューを依頼する。
//
// ループ構造は mgr と同じ Inbox → think → work → report → idle（§6.3）。
// LLM 応答の JSON 解析に失敗した場合は決定的なフォールバックを使い、
// worker 不在・エラー時も Run ループを落とさない。
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yukirawa/ai-office/server/internal/llm"
	"github.com/yukirawa/ai-office/server/internal/persona"
)

const (
	// defaultBaseBranch は remote モードの既定ベースブランチ（§13.6）。
	defaultBaseBranch = "main"
	// reportDir はフォールバック成果物を置くディレクトリ。
	reportDir = "reports"
	// defaultTaskTimeout は worker の実行結果を待つ上限。
	// worker が無応答・切断した場合に Await が永久にブロックしないための保険。
	defaultTaskTimeout = 5 * time.Minute
)

// devJSONOnlyInstruction は dev に厳密 JSON を要求する共通指示（§13.2）。
const devJSONOnlyInstruction = "必ず JSON のみを返してください。"

// devLocalInstruction は local モード用の追加システムプロンプト。
const devLocalInstruction = `あなたは開発担当です。タスクを遂行するために worker に実行させるファイル操作を JSON で指示してください。形式は {"actions":[{"op":"write","path":"reports/x.md","content":"..."}]} です。op は read|write|list|exec のいずれかです。`

// devRemoteInstruction は remote モード用の追加システムプロンプト。
const devRemoteInstruction = `あなたは開発担当です。GitHub へ提出するファイルを JSON で指示してください。形式は {"files":[{"path":"...","content":"..."}]} です。`

// devStepInstruction は反復ループ用のシステムプロンプト。
// 実行結果を踏まえて「次に行うこと」だけを JSON で返させる（§16 の自律実行）。
const devStepInstruction = `あなたは開発担当です。これまでの実行結果を踏まえ、次に行うことだけを JSON で答えてください。
- 作業を進める: {"actions":[{"op":"write|read|list|exec",...}]}（複数可）
- 完了した: {"done":true,"summary":"1行の要約"}
- 情報が足りない: {"question":"確認したいこと"}
ファイルを作る場合は必ず write を含めてください。既存の構成は list/read で確認し、無いファイルを読んで失敗したら別の手段に切り替えてください。`

// planResult は LLM 応答から抽出した割当内容。local は Actions、remote は Files を使う。
// Question が入っている場合は「情報不足でオーナーに確認したい」という意思表示（§16）。
// Done が true の場合はタスク完了の申告。
type planResult struct {
	Actions  []Action     `json:"actions"`
	Files    []RemoteFile `json:"files"`
	Question string       `json:"question,omitempty"`
	Done     bool         `json:"done,omitempty"`
	Summary  string       `json:"summary,omitempty"`
}

// isEmpty はどの指示も含まれていない（＝解釈できなかった）かを返す。
func (p planResult) isEmpty() bool {
	return !p.Done && strings.TrimSpace(p.Question) == "" && len(p.Actions) == 0 && len(p.Files) == 0
}

// DevAgent は 1 体の開発担当 AI 社員を動かす。1 体 = 1 goroutine を想定する。
type DevAgent struct {
	id   string
	name string

	client   llm.Client
	notifier Notifier
	persona  persona.Persona
	opts     Options

	dispatcher Dispatcher
	updater    TaskUpdater
	remote     RemoteExecutor
	reviewer   Reviewer

	// escalator は疑問を mgr 経由でオーナーへ上げる先（mgr を SetEscalator で設定）。
	escalator Escalator

	inbox chan Task

	mu    sync.RWMutex
	state string
}

// NewDevAgent は DevAgent を生成する。Run を別 goroutine で呼ぶまでタスクは処理されない。
func NewDevAgent(id string, p persona.Persona, client llm.Client, notifier Notifier,
	dispatcher Dispatcher, updater TaskUpdater, remote RemoteExecutor, reviewer Reviewer, opts Options) *DevAgent {
	opts = normalizeOptions(opts)
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}
	return &DevAgent{
		id:         id,
		name:       name,
		client:     client,
		notifier:   notifier,
		persona:    p,
		opts:       opts,
		dispatcher: dispatcher,
		updater:    updater,
		remote:     remote,
		reviewer:   reviewer,
		inbox:      make(chan Task, inboxCapacity),
		state:      StateIdle,
	}
}

// ID は社員 ID を返す。
func (d *DevAgent) ID() string { return d.id }

// SetEscalator は疑問を上げる先（mgr）を設定する。構築後でも呼べる（§16）。
func (d *DevAgent) SetEscalator(e Escalator) { d.escalator = e }

// State は現在の状態を返す（スレッドセーフ）。
func (d *DevAgent) State() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

// Inbox はタスクを送るためのチャンネルを公開する。
func (d *DevAgent) Inbox() chan<- Task { return d.inbox }

// Post はタスクを受信箱にノンブロッキングで積む。満杯またはクローズ済みなら false。
func (d *DevAgent) Post(t Task) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case d.inbox <- t:
		return true
	default:
		return false
	}
}

// Run は Inbox を処理し続ける。起動時に状態を idle にする。
// 出勤の挨拶は worker 側が行うため、ここでは重複する通知を出さない。
func (d *DevAgent) Run(ctx context.Context) {
	d.setState(StateIdle)
	d.logger().Info("dev エージェントを起動しました", "id", d.id, "name", d.name)

	for {
		select {
		case <-ctx.Done():
			d.logger().Info("コンテキスト終了のため dev エージェントを停止します", "id", d.id)
			return
		case t, ok := <-d.inbox:
			if !ok {
				d.logger().Info("受信箱が閉じられたため dev エージェントを停止します", "id", d.id)
				return
			}
			d.handleTask(ctx, t)
		}
	}
}

// handleTask は panic を吸収して Run ループを守る。
func (d *DevAgent) handleTask(ctx context.Context, t Task) {
	defer func() {
		if r := recover(); r != nil {
			d.logger().Error("タスク処理中に panic しました", "id", d.id, "task_id", t.ID, "panic", r)
			d.setState(StateIdle)
		}
	}()
	d.runTask(ctx, t)
}

// runTask は 1 タスクを think → work → report → idle の順に処理する。
func (d *DevAgent) runTask(ctx context.Context, t Task) {
	d.setState(StateThinking)
	d.updateTask(ctx, t.ID, "working", "")

	res := d.work(ctx, t)
	if res.TaskID == "" {
		res.TaskID = t.ID
	}

	d.updateTask(ctx, t.ID, "review", res.Summary)
	d.notify(ctx, fmt.Sprintf("タスク「%s」: %s — %s", taskLabel(t), res.Status, res.Summary))
	if d.reviewer != nil {
		d.reviewer.Review(ctx, t, res)
	}
	d.setState(StateIdle)
}

// work はタスクを実行して結果を返す。remote は単発、local は LLM と対話しながら反復する（§16）。
func (d *DevAgent) work(ctx context.Context, t Task) Result {
	if strings.TrimSpace(t.Mode) == "remote" || d.client == nil {
		// remote は PR 単位の単発実行。LLM が無いときも従来のフォールバック（レポート 1 件）にする。
		assign, guard := d.buildAssign(ctx, t)
		d.warnGuard(t, guard)
		d.setState(StateWorking)
		res := d.execute(ctx, t, assign)
		if res.TaskID == "" {
			res.TaskID = t.ID
		}
		return res
	}
	return d.localLoop(ctx, t)
}

// warnGuard は無限ループ防止ガードの作動をログに記録する。
func (d *DevAgent) warnGuard(t Task, guard string) {
	if guard == "" {
		return
	}
	d.logger().Warn("無限ループ防止ガードが作動しました",
		"id", d.id, "task_id", t.ID, "guard", guard,
		"max_turns", d.opts.MaxTurns, "token_budget", d.opts.TokenBudget)
}

// localLoop は「LLM に次の一手を聞く → 実行する → 結果を渡して繰り返す」自律ループ（§16）。
// 明示的な done、ターン上限、トークン予算で終了する。
func (d *DevAgent) localLoop(ctx context.Context, t Task) Result {
	observation := ""
	lastSummary := ""
	tokensUsed := 0

	for round := 1; round <= d.opts.MaxTurns; round++ {
		if err := ctx.Err(); err != nil {
			return failedResult(t.ID, err)
		}

		step, used, err := d.planStep(ctx, t, observation)
		tokensUsed += used
		if err != nil {
			d.logger().Warn("dev の計画 LLM 呼び出しに失敗しました",
				"id", d.id, "task_id", t.ID, "round", round, "error", err)
			break
		}

		// エージェント形式で応答しないモデル向けの後方互換: 初回が解釈不能なら従来の単発フォールバック。
		if round == 1 && step.isEmpty() {
			assign, guard := d.buildAssign(ctx, t)
			d.warnGuard(t, guard)
			d.setState(StateWorking)
			res := d.execute(ctx, t, assign)
			if res.TaskID == "" {
				res.TaskID = t.ID
			}
			return res
		}

		switch {
		case step.Done:
			if s := strings.TrimSpace(step.Summary); s != "" {
				lastSummary = s
			}
			if lastSummary == "" {
				lastSummary = "作業を完了しました"
			}
			return Result{TaskID: t.ID, Status: "done", Summary: lastSummary, Detail: observation}

		case strings.TrimSpace(step.Question) != "":
			if answer, ok := d.escalate(ctx, t, step.Question); ok {
				observation = appendObservation(observation, "オーナーの回答: "+answer)
			}

		case len(step.Actions) > 0:
			assign := TaskAssign{
				TaskID: t.ID, Mode: "local", Title: t.Title, Reason: t.Plan,
				Workspace: t.Workspace, Actions: step.Actions,
			}
			d.setState(StateWorking)
			res := d.execute(ctx, t, assign)
			if s := strings.TrimSpace(res.Summary); s != "" {
				lastSummary = s
			}
			observation = appendObservation(observation, formatObservation(res))

		default:
			if lastSummary == "" {
				lastSummary = "進められる作業がありませんでした"
			}
			return Result{TaskID: t.ID, Status: "done", Summary: lastSummary, Detail: observation}
		}

		if tokensUsed > d.opts.TokenBudget {
			d.logger().Warn("dev のトークン予算を超過しました",
				"id", d.id, "task_id", t.ID, "tokens_used", tokensUsed)
			break
		}
	}

	if lastSummary == "" {
		lastSummary = "ターン上限または予算超過で終了しました"
	}
	return Result{TaskID: t.ID, Status: "done", Summary: lastSummary, Detail: observation}
}

// planStep は「次の一手」を LLM に 1 回問い合わせて解釈する。
func (d *DevAgent) planStep(ctx context.Context, t Task, observation string) (planResult, int, error) {
	req := llm.Request{
		Model:     d.opts.Model,
		System:    d.stepSystemPrompt(),
		Messages:  []llm.Message{{Role: "user", Content: stepPrompt(t, observation)}},
		MaxTokens: d.opts.MaxTokens,
	}
	resp, err := d.client.Chat(ctx, req)
	if err != nil {
		return planResult{}, 0, err
	}
	used := resp.InputTokens + resp.OutputTokens
	if parsed, ok := parseAssign(resp.Text); ok {
		return parsed, used, nil
	}
	return planResult{}, used, nil
}

// stepSystemPrompt は反復ループ用のシステムプロンプトを組み立てる。
func (d *DevAgent) stepSystemPrompt() string {
	parts := make([]string, 0, 3)
	if base := strings.TrimSpace(d.persona.SystemPrompt()); base != "" {
		parts = append(parts, base)
	}
	parts = append(parts, devStepInstruction, devJSONOnlyInstruction)
	return strings.Join(parts, "\n\n")
}

// stepPrompt は「次の一手」を問うユーザーメッセージを組み立てる。
func stepPrompt(t Task, observation string) string {
	var b strings.Builder
	b.WriteString("次のタスクを進めてください。\n")
	if t.Title != "" {
		fmt.Fprintf(&b, "タイトル: %s\n", t.Title)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "内容: %s\n", t.Description)
	}
	if t.From != "" {
		fmt.Fprintf(&b, "依頼者: %s\n", t.From)
	}
	if p := strings.TrimSpace(t.Plan); p != "" {
		fmt.Fprintf(&b, "mgr の計画: %s\n", p)
	}
	if ws := strings.TrimSpace(t.Workspace); ws != "" {
		fmt.Fprintf(&b, "作業ディレクトリ: %s\n", ws)
	}
	if o := strings.TrimSpace(observation); o != "" {
		b.WriteString("\nこれまでの実行結果:\n")
		b.WriteString(truncateText(o, 4000))
		b.WriteString("\n")
	} else {
		b.WriteString("\n(まだ作業を開始していません)\n")
	}
	b.WriteString("\n次に行うことだけを JSON で返してください。")
	return b.String()
}

// appendObservation は観察ログを追記し、古い部分を切り詰めて長さを抑える。
func appendObservation(prev, add string) string {
	const maxObs = 6000
	s := prev
	if s != "" {
		s += "\n---\n"
	}
	s += add
	if r := []rune(s); len(r) > maxObs {
		s = string(r[len(r)-maxObs:])
	}
	return s
}

// formatObservation はタスク結果を次の一手の材料になる形に整える。
func formatObservation(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "実行結果: %s — %s", res.Status, res.Summary)
	if d := strings.TrimSpace(res.Detail); d != "" {
		b.WriteString("\n")
		b.WriteString(d)
	}
	return b.String()
}

// truncateText は s を最大 n ルーンへ切り詰める。
func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// execute は worker へ割り当てて結果を待つ。remote で dispatch に失敗した場合は
// RemoteExecutor にフォールバックする（§13.5）。
// 待機には TaskTimeout を設け、worker の無応答で永久にブロックしないようにする。
func (d *DevAgent) execute(ctx context.Context, t Task, assign TaskAssign) Result {
	timeout := d.opts.TaskTimeout
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dispatchErr error
	if d.dispatcher == nil {
		dispatchErr = errors.New("dispatcher が設定されていません")
	} else {
		dispatchErr = d.dispatcher.Dispatch(execCtx, d.id, assign)
	}

	if dispatchErr != nil {
		if assign.Mode == "remote" && assign.Remote != nil && d.remote != nil {
			d.logger().Warn("dispatch に失敗したため RemoteExecutor で実行します",
				"id", d.id, "task_id", t.ID, "error", dispatchErr)
			res, err := d.remote.ExecuteRemote(execCtx, *assign.Remote)
			if err != nil {
				return failedResult(t.ID, err)
			}
			return res
		}
		return failedResult(t.ID, dispatchErr)
	}

	res, err := d.dispatcher.Await(execCtx, t.ID)
	if err != nil {
		return failedResult(t.ID, err)
	}
	return res
}

// buildAssign はタスクを TaskAssign へ変換する。LLM が使えればその JSON を尊重し、
// 失敗時は決定的なフォールバック（レポート 1 ファイル）を使う。
func (d *DevAgent) buildAssign(ctx context.Context, t Task) (TaskAssign, string) {
	report := reportContent(t)
	remote := strings.TrimSpace(t.Mode) == "remote"

	var (
		actions []Action
		files   []RemoteFile
		guard   string
	)
	if d.client != nil {
		var question string
		actions, files, guard, question = d.planWithLLM(ctx, t, remote, "")
		if strings.TrimSpace(question) != "" {
			// 情報不足。mgr 経由でオーナーへ確認し、回答を得たらそれを踏まえて計画し直す（§16）。
			if answer, ok := d.escalate(ctx, t, question); ok {
				actions, files, guard, _ = d.planWithLLM(ctx, t, remote, answer)
			}
		}
	}

	if remote {
		if len(files) == 0 {
			files = []RemoteFile{{Path: reportPath(t), Content: report}}
		}
		return TaskAssign{
			TaskID:    t.ID,
			Mode:      "remote",
			Title:     t.Title,
			Reason:    t.Plan,
			Workspace: t.Workspace,
			Remote: &RemoteSpec{
				Repo:       t.Repo,
				BaseBranch: baseBranch(t),
				Branch:     branchName(t.ID),
				Title:      t.Title,
				Body:       report,
				Files:      files,
				// Token は api が Dispatch 時に発行するため、ここでは設定しない。
			},
		}, guard
	}

	if len(actions) == 0 {
		actions = []Action{{Op: "write", Path: reportPath(t), Content: report}}
	}
	return TaskAssign{TaskID: t.ID, Mode: "local", Title: t.Title, Reason: t.Plan, Workspace: t.Workspace, Actions: actions}, guard
}

// escalate は担当者の疑問を mgr 経由でオーナーへ上げ、回答を待つ（§16）。
// 回答が得られない場合は ok=false（呼び先はフォールバックで作業を続ける）。
func (d *DevAgent) escalate(ctx context.Context, t Task, question string) (string, bool) {
	if d.escalator == nil {
		d.logger().Warn("エスカレーション先が未設定のため質問できません", "id", d.id, "task_id", t.ID)
		return "", false
	}
	d.setState(StateThinking)
	d.notify(ctx, fmt.Sprintf("%s: 確認したい点があるため、mgr 経由でオーナーに質問します。", d.name))

	answer, ok := d.escalator.Escalate(ctx, Question{FromID: d.id, TaskID: t.ID, Text: question})
	if !ok {
		d.notify(ctx, "（回答が得られなかったため、ひとまず進めます）")
		return "", false
	}
	d.notify(ctx, "オーナーの回答を受け取りました。反映して作業を続けます。")
	return answer, true
}

// planWithLLM は LLM に厳密 JSON を要求し、解析できたら割当内容を返す。
// 上位が情報不足を意味する JSON（{"question":"..."}）を返した場合は、question にその文言を入れる。
// extra には前回の質問への回答など、プロンプトに足す追加情報を渡す。
// 無限ループ防止（最大ターン数・トークン予算・同一結論検出）を mgr と同じ規律で適用する。
func (d *DevAgent) planWithLLM(ctx context.Context, t Task, remote bool, extra string) ([]Action, []RemoteFile, string, string) {
	req := llm.Request{
		Model:     d.opts.Model,
		System:    d.systemPrompt(remote),
		Messages:  []llm.Message{{Role: "user", Content: assignPrompt(t, remote, extra)}},
		MaxTokens: d.opts.MaxTokens,
	}

	tokensUsed := 0
	repeats := make(map[string]int, repeatConclusionLimit)
	guard := ""

	for turn := 1; turn <= d.opts.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, "", ""
		}

		resp, err := d.client.Chat(ctx, req)
		if err != nil {
			d.logger().Warn("dev の計画 LLM 呼び出しに失敗しました。フォールバックします",
				"id", d.id, "task_id", t.ID, "turn", turn, "error", err)
			return nil, nil, "", ""
		}

		tokensUsed += resp.InputTokens + resp.OutputTokens

		if parsed, ok := parseAssign(resp.Text); ok {
			if q := strings.TrimSpace(parsed.Question); q != "" {
				return nil, nil, "", q
			}
			if len(parsed.Actions) > 0 || len(parsed.Files) > 0 {
				return parsed.Actions, parsed.Files, "", ""
			}
		}

		// ガード1: 同一結論の繰り返し検出。
		if text := strings.TrimSpace(resp.Text); text != "" {
			key := hashText(text)
			repeats[key]++
			if repeats[key] >= repeatConclusionLimit {
				guard = "repeated_conclusion"
				break
			}
		}

		// ガード2: トークン予算超過。
		if tokensUsed > d.opts.TokenBudget {
			guard = "token_budget"
			break
		}

		// 終端理由なら、JSON が得られなくてもフォールバックへ回す。
		if isTerminalStop(resp.StopReason) {
			break
		}

		req.Messages = append(req.Messages,
			llm.Message{Role: "assistant", Content: resp.Text},
			llm.Message{Role: "user", Content: continueInstruction},
		)

		// ガード3: 最大ターン数。
		if turn == d.opts.MaxTurns {
			guard = "max_turns"
		}
	}
	return nil, nil, guard, ""
}

// systemPrompt はペルソナに dev 用の指示と厳密 JSON 要求を足して返す。
func (d *DevAgent) systemPrompt(remote bool) string {
	instruction := devLocalInstruction
	if remote {
		instruction = devRemoteInstruction
	}
	parts := make([]string, 0, 3)
	if base := strings.TrimSpace(d.persona.SystemPrompt()); base != "" {
		parts = append(parts, base)
	}
	parts = append(parts, instruction, devJSONOnlyInstruction)
	return strings.Join(parts, "\n\n")
}

// converseSystemPrompt はペルソナに会話用の指示を足して返す（Converse 用）。
func (d *DevAgent) converseSystemPrompt() string {
	return buildConverseSystemPrompt(d.persona.SystemPrompt(), d.name)
}

// Converse は #会議室 などでの 1 往復の会話に応答する（Converser）。
// タスク受信箱とは独立に、その場で LLM を呼んでチャンネルへ投稿する。
func (d *DevAgent) Converse(ctx context.Context, channel, prompt string) {
	if strings.TrimSpace(channel) == "" {
		channel = d.opts.Channel
	}
	ctx, cancel := context.WithTimeout(ctx, converseTimeout(d.opts))
	defer cancel()

	d.setState(StateThinking)
	if d.client == nil {
		d.logger().Error("LLM クライアントが未設定のため会話に応答できません", "id", d.id)
		d.setState(StateIdle)
		return
	}

	text, ok := converseRun(ctx, d.client, d.logger(), d.id, d.opts.Model, d.converseSystemPrompt(), prompt, d.opts)
	if !ok {
		d.setState(StateIdle)
		return
	}

	reply := sanitizeChatReply(text)
	if reply == "" {
		reply = converseFallbackReply
	}
	d.notifyChannel(ctx, channel, reply)
	d.setState(StateIdle)
}

// Initiative は dev の自発行動（§16 自律）。気づきや提案、mgr への確認を行う。
func (d *DevAgent) Initiative(ctx context.Context, brief string) string {
	if d.State() != StateIdle {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, converseTimeout(d.opts))
	defer cancel()

	d.setState(StateThinking)
	defer d.setState(StateIdle)

	system := initiativeSystemPrompt(d.persona.SystemPrompt(), d.name, "開発担当")
	text, _ := initiativeRun(ctx, d.client, d.logger(), d.id, d.opts.Model, system, brief, d.opts)
	return text
}

// assignPrompt はタスクを LLM に渡すユーザーメッセージに整形する。
// extra には前回の質問へのオーナーの回答などを差し込む（§16）。
func assignPrompt(t Task, remote bool, extra string) string {
	var b strings.Builder
	b.WriteString("次のタスクを遂行するための JSON を作成してください。\n")
	if t.ID != "" {
		fmt.Fprintf(&b, "タスクID: %s\n", t.ID)
	}
	if t.Title != "" {
		fmt.Fprintf(&b, "タイトル: %s\n", t.Title)
	}
	if t.From != "" {
		fmt.Fprintf(&b, "依頼者: %s\n", t.From)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "内容: %s\n", t.Description)
	}
	if p := strings.TrimSpace(t.Plan); p != "" {
		fmt.Fprintf(&b, "mgr の計画: %s\n", p)
	}
	if e := strings.TrimSpace(extra); e != "" {
		fmt.Fprintf(&b, "オーナーからの回答: %s\n", e)
	}
	b.WriteString("\n情報が足りない場合は {\"question\":\"確認したいこと\"} を返してください。")
	if remote {
		b.WriteString("揃っていれば {\"files\":[{\"path\":\"...\",\"content\":\"...\"}]} の形式の JSON だけを返してください。")
	} else {
		b.WriteString("揃っていれば {\"actions\":[{\"op\":\"write\",\"path\":\"...\",\"content\":\"...\"}]} の形式の JSON だけを返してください。")
	}
	return b.String()
}

// parseAssign は LLM 応答から最初の JSON オブジェクトを抜き出して解析する。
// ```json フェンスや前後の説明文があっても壊れないよう、最外側の {...} を探す。
func parseAssign(text string) (planResult, bool) {
	candidate := extractJSONObject(text)
	if candidate == "" {
		return planResult{}, false
	}
	var pr planResult
	if err := json.Unmarshal([]byte(candidate), &pr); err != nil {
		return planResult{}, false
	}
	// 単一アクションの省略形（{"op":"write",...}）にも対応する。
	if len(pr.Actions) == 0 && len(pr.Files) == 0 {
		var single Action
		if err := json.Unmarshal([]byte(candidate), &single); err == nil && single.Op != "" {
			pr.Actions = []Action{single}
		}
	}
	return pr, true
}

// extractJSONObject はコードフェンスを除去し、最外側の {...} を 1 つ返す。
// 文字列リテラル内の波括弧は数えない。
func extractJSONObject(s string) string {
	s = stripCodeFence(s)
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// stripCodeFence は前後の Markdown コードフェンスを除去する。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return s
	}
	s = s[i+1:]
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// reportContent はフォールバック用の日本語レポートを組み立てる。
func reportContent(t Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", taskLabel(t))

	b.WriteString("## 概要\n\n")
	if desc := strings.TrimSpace(t.Description); desc != "" {
		b.WriteString(desc + "\n\n")
	} else {
		b.WriteString("(説明なし)\n\n")
	}

	b.WriteString("## 計画\n\n")
	if plan := strings.TrimSpace(t.Plan); plan != "" {
		b.WriteString(plan + "\n\n")
	} else {
		b.WriteString("(計画なし)\n\n")
	}

	fmt.Fprintf(&b, "## 生成時刻\n\n%s\n", time.Now().Format(time.RFC3339))
	return b.String()
}

// reportPath はフォールバック成果物のパス reports/<task_id>.md を返す。
// ID にパス区切りが混じってもディレクトリを抜けないようサニタイズする。
func reportPath(t Task) string {
	id := strings.TrimSpace(t.ID)
	if id == "" {
		id = "task"
	}
	id = pathSafe(id)
	return reportDir + "/" + id + ".md"
}

// branchName は remote モードで作るブランチ名を返す。
func branchName(taskID string) string {
	id := strings.TrimSpace(taskID)
	if id == "" {
		id = "task"
	}
	return "ai-office/task-" + pathSafe(id)
}

// baseBranch は remote モードのベースブランチを返す（未指定は main）。
func baseBranch(t Task) string {
	if b := strings.TrimSpace(t.BaseBranch); b != "" {
		return b
	}
	return defaultBaseBranch
}

// pathSafe はパス区切りと親ディレクトリ参照を無害化する。
func pathSafe(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_")
	return r.Replace(s)
}

// failedResult は失敗を表す Result を組み立てる。
func failedResult(taskID string, err error) Result {
	msg := "不明なエラー"
	if err != nil {
		msg = err.Error()
	}
	return Result{TaskID: taskID, Status: "failed", Summary: "実行できませんでした: " + msg}
}

// updateTask はタスク状態を更新する。エラーはログに記録するだけで握りつぶす。
func (d *DevAgent) updateTask(ctx context.Context, taskID, status, result string) {
	if d.updater == nil {
		return
	}
	if err := d.updater.UpdateTask(ctx, taskID, status, result); err != nil {
		d.logger().Error("タスク状態の更新に失敗しました", "task_id", taskID, "status", status, "error", err)
	}
}

// setState は内部状態を更新し、Notifier に公開する。
func (d *DevAgent) setState(s string) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()

	if d.notifier != nil {
		d.notifier.SetAgentState(d.id, s)
	}
}

// notify は既定チャンネル（Options.Channel）へ投稿する。
func (d *DevAgent) notify(ctx context.Context, text string) {
	d.notifyChannel(ctx, d.opts.Channel, text)
}

// notifyChannel は指定チャンネルへ投稿する。エラーはログに記録して握りつぶす。
func (d *DevAgent) notifyChannel(ctx context.Context, channel, text string) {
	if d.notifier == nil {
		return
	}
	if err := d.notifier.Notify(ctx, channel, d.id, text); err != nil {
		d.logger().Error("通知の投稿に失敗しました", "channel", channel, "error", err)
	}
}

func (d *DevAgent) logger() *slog.Logger {
	return d.opts.Logger
}
