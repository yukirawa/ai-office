//! ai-office TUI（Phase 0.4 + 入力対応）。
//!
//! `/ws` に接続し、サーバーから届く [`ServerMsg`] を画面に表示する。
//! 下部の入力行から #会議室 への発言（`say`）とタスク投入（`task`）ができる（§15）。
//!
//! 環境変数:
//! - `OFFICE_SERVER_URL` 既定 `ws://127.0.0.1:8787/ws`
//! - `OFFICE_TUI_ID`   既定 `owner`
//!
//! ヘッドレススモークモード:
//! - `OFFICE_TUI_SNAPSHOT=1` または第1引数 `--snapshot` で、代替スクリーンに入らず
//!   約2秒収集してプレーンテキストのサマリを stdout に出し、exit 0 する（入力は使わない）。
//! - `--say <本文>`（複数回指定可、順に送信）を付けると、hello 後に #会議室 へ発言してから
//!   同様に約2秒収集してサマリを出す。`--say` だけでもヘッドレスで起動し、`--snapshot` と併用できる。
//!   `--say=本文` 形式にも対応する。自動テストやスクリプトから `@宛先` を 1 回叩く用途を想定。

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::Result;
use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};
use futures_util::stream::{SplitSink, SplitStream};
use futures_util::{SinkExt, StreamExt};
use protocol::{ClientMsg, EmployeeInfo, RelationshipInfo, ServerMsg, TaskInfo};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, List, ListItem, Paragraph};
use ratatui::{DefaultTerminal, Frame};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, watch};
use tokio::time::{Instant, interval_at, sleep, timeout};
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

const DEFAULT_URL: &str = "ws://127.0.0.1:8787/ws";
const DEFAULT_TUI_ID: &str = "owner";
const DEFAULT_VERSION: &str = "0.1.0";
const BACKOFF_BASE_SECS: u64 = 1;
const BACKOFF_MAX_SECS: u64 = 30;
/// 保持する notice の最大件数（無限成長を防ぐ）。
const MAX_NOTICES: usize = 500;
/// タスクペインに表示する最大件数。
const MAX_TASK_ROWS: usize = 8;
/// スナップショットモードの収集秒数。
const SNAPSHOT_SECS: u64 = 2;
/// `--say` 時、最初の welcome を待つ最大時間。届かなくても発言は試みる。
const WELCOME_TIMEOUT: Duration = Duration::from_secs(2);
/// フレーム更新間隔（約10fps）。
const FRAME_INTERVAL: Duration = Duration::from_millis(100);
/// heartbeat の送信間隔（§4.1 の推奨 30 秒）。
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
/// 発言の既定チャンネル（§15.1）。
const DEFAULT_CHANNEL: &str = "#会議室";
/// 入力行に出すヒント。
const INPUT_HINT: &str = "[Enter] 送信  [PgUp/PgDn] スクロール  [@mgr/@all 宛先]  [/task タイトル]  [/help]  [/quit or Esc] 終了";
/// 入力行の右端に出す接続状態の最大表示幅。
const MAX_STATUS_WIDTH: u16 = 28;
/// 入力行のヒントに使う最大幅（入力を圧迫しないように上限制）。
const HINT_MAX_WIDTH: u16 = 60;
/// 入力欄に最低限残す幅（プロンプト込み）。
const INPUT_MIN_WIDTH: u16 = 16;
/// `/task` にタイトルが無いとき（または `-d` の値が無いとき）の使い方。
const TASK_USAGE: &str = "使い方: /task [-d <作業先>] <タイトル>";
/// `/help` の表示内容。
const HELP_LINES: &[&str] = &[
    "コマンド:",
    "  /task [-d <作業先>] <タイトル>  タスクを依頼（-d で作業ディレクトリ指定）",
    "  /help             このヘルプを表示",
    "  /quit, /exit      終了",
    "  /say <本文>       質問保留中でも #会議室 へ発言",
    "  /skip, /cancel    保留中の質問への回答をやめる",
    "  @mgr <本文>       mgr 宛てに発言 (例: @mgr 点呼)",
    "  @all <本文>       全員宛てに発言",
    "  その他の入力      #会議室 へ発言",
    "  質問保留中        通常入力は質問への回答として送信 (回答> 表示)",
    "キー: Enter=送信 / PageUp・PageDown=スクロール / Home=最古 / End=最新",
    "      Ctrl-U=クリア / Esc・Ctrl-C=終了 (q は入力文字)",
];

/// 3 ペイン（社員・会議室・mgr）を出す最小幅。これ未満では右ペインを隠す。
const THREE_PANE_MIN_WIDTH: u16 = 56;
/// 左列（社員/タスク）を出す最小幅。これ未満では会議室だけを全幅で見せる。
const TWO_PANE_MIN_WIDTH: u16 = 28;
/// 左列の最小幅。
const LEFT_MIN_WIDTH: u16 = 12;
/// 中央（#会議室）の最小幅。
const CENTER_MIN_WIDTH: u16 = 16;
/// 右（mgr）の最小幅。
const RIGHT_MIN_WIDTH: u16 = 16;
/// 入力行と枠線を除いた、会議室ペインの概算高さ（ページ送り量の計算用）。
const CHROME_ROWS: u16 = 3;

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;
type WsSink = SplitSink<WsStream, Message>;
type WsSource = SplitStream<WsStream>;

/// TUI 設定。
#[derive(Debug, Clone)]
struct Config {
    url: String,
    tui_id: String,
    version: String,
}

impl Config {
    fn from_env() -> Self {
        Self {
            url: env_string("OFFICE_SERVER_URL", DEFAULT_URL),
            tui_id: env_string("OFFICE_TUI_ID", DEFAULT_TUI_ID),
            version: env_string("OFFICE_VERSION", DEFAULT_VERSION),
        }
    }
}

/// notice ログの1行。
#[derive(Debug, Clone)]
struct NoticeLine {
    channel: String,
    from: String,
    text: String,
    ts: String,
    /// TUI ローカルの表示（ヘルプ / システム行 / タスク投入の確認）。サーバー由来ではない。
    local: bool,
}

/// 接続状態。
#[derive(Debug, Clone)]
enum ConnectionStatus {
    Connecting,
    Connected,
    Disconnected(String),
}

impl ConnectionStatus {
    fn label(&self) -> String {
        match self {
            ConnectionStatus::Connecting => "接続中...".to_string(),
            ConnectionStatus::Connected => "接続済み".to_string(),
            ConnectionStatus::Disconnected(reason) => {
                if reason.is_empty() {
                    "接続が切れました。再接続中...".to_string()
                } else {
                    format!("接続が切れました。再接続中... ({reason})")
                }
            }
        }
    }

    fn color(&self) -> Color {
        match self {
            ConnectionStatus::Connecting => Color::Yellow,
            ConnectionStatus::Connected => Color::Green,
            ConnectionStatus::Disconnected(_) => Color::Red,
        }
    }
}

/// 共有アプリ状態。
#[derive(Debug)]
struct App {
    online: Vec<String>,
    employees: Vec<EmployeeInfo>,
    ledger: BTreeMap<String, i64>,
    relationships: Vec<RelationshipInfo>,
    /// 最新の office_state が持つタスク一覧（新しい順。追記ではなく置換）。
    tasks: Vec<TaskInfo>,
    notices: Vec<NoticeLine>,
    /// 会議室のスクロール位置。下（最新）からのメッセージ数で、0 は最新を追従。
    scroll: usize,
    /// 下部の入力バッファ（カーソルは常に末尾）。
    input: String,
    /// 未回答の質問 id。`Some` の間は通常入力を回答として送る。
    pending_question: Option<String>,
    status: ConnectionStatus,
}

impl Default for App {
    fn default() -> Self {
        Self {
            online: Vec::new(),
            employees: Vec::new(),
            ledger: BTreeMap::new(),
            relationships: Vec::new(),
            tasks: Vec::new(),
            notices: Vec::new(),
            scroll: 0,
            input: String::new(),
            pending_question: None,
            status: ConnectionStatus::Connecting,
        }
    }
}

impl App {
    /// 受信メッセージで状態を更新する。
    fn apply(&mut self, msg: &ServerMsg) {
        match msg {
            ServerMsg::Welcome { office, .. } => {
                self.online = office.online.clone();
                // 再接続のたびにサーバーが welcome -> snapshot -> 直近ログ の順で送り直す。
                // 直前の会話を捨ててから履歴を受け直すことで、重複表示を防ぐ。
                self.notices.clear();
                self.scroll = 0;
            }
            ServerMsg::Notice {
                channel,
                from,
                text,
                ts,
            } => {
                self.notices.push(NoticeLine {
                    channel: channel.clone(),
                    from: from.clone(),
                    text: text.clone(),
                    ts: ts.clone(),
                    local: false,
                });
                self.trim_notices();
            }
            ServerMsg::OfficeState {
                online,
                employees,
                ledger,
                relationships,
                tasks,
                ..
            } => {
                self.online = online.clone();
                self.employees = employees.clone();
                self.ledger = ledger.clone();
                self.relationships = relationships.clone();
                // スナップショットは最新状態なので、追記せず置換する。
                self.tasks = tasks.clone();
            }
            ServerMsg::Question {
                id, from, text, ts, ..
            } => {
                // 質問は #会議室 に見える行として追加する（送信者名に印を付ける）。
                self.notices.push(NoticeLine {
                    channel: DEFAULT_CHANNEL.to_string(),
                    from: format!("{from}（質問）"),
                    text: text.clone(),
                    ts: ts.clone(),
                    local: false,
                });
                self.trim_notices();
                // 未回答の質問 id を保持する。welcome による notices クリアとは独立。
                self.pending_question = Some(id.clone());
            }
            ServerMsg::TaskAssign { .. }
            | ServerMsg::TaskCancel { .. }
            | ServerMsg::Error { .. }
            | ServerMsg::Unknown => {}
        }
    }

    /// TUI ローカルの行（ヘルプ / システム行 / タスク投入の確認）を追加する。
    ///
    /// 発言（say）のエコーには使わない。送信者自身にもサーバーから notice が届くため、
    /// ここで足すと二重表示になり、再接続時の履歴とも順序がずれる。
    fn push_local(&mut self, text: String) {
        self.notices.push(NoticeLine {
            channel: DEFAULT_CHANNEL.to_string(),
            from: "owner".to_string(),
            text,
            ts: String::new(),
            local: true,
        });
        self.trim_notices();
    }

    /// notice の保持件数を [`MAX_NOTICES`] に収める。
    fn trim_notices(&mut self) {
        if self.notices.len() > MAX_NOTICES {
            let excess = self.notices.len() - MAX_NOTICES;
            self.notices.drain(0..excess);
            // 先頭を捨てたぶん、行き過ぎたスクロール位置を戻す。
            self.scroll = self.scroll.min(self.max_scroll());
        }
    }

    /// 会議室スクロールの上限（最古まで）。0 件なら 0。
    fn max_scroll(&self) -> usize {
        self.notices.len().saturating_sub(1)
    }

    /// 上（過去）へ `n` 件スクロールする。上限でクランプする。
    fn scroll_up(&mut self, n: usize) {
        self.scroll = self.scroll.saturating_add(n).min(self.max_scroll());
    }

    /// 下（最新）へ `n` 件スクロールする。0 でクランプする。
    fn scroll_down(&mut self, n: usize) {
        self.scroll = self.scroll.saturating_sub(n);
    }

    /// 最古の位置までスクロールする。
    fn scroll_to_oldest(&mut self) {
        self.scroll = self.max_scroll();
    }

    /// 最新に戻って追従する。
    fn scroll_to_newest(&mut self) {
        self.scroll = 0;
    }

    /// 左ペインに出す社員一覧。`employees` が空なら `online` から合成する。
    fn employee_rows(&self) -> Vec<EmployeeInfo> {
        if !self.employees.is_empty() {
            return self.employees.clone();
        }
        self.online
            .iter()
            .map(|id| EmployeeInfo {
                id: id.clone(),
                name: id.clone(),
                role: String::new(),
                status: "online".to_string(),
                state: String::new(),
            })
            .collect()
    }

    /// mgr の状態文字列。
    fn mgr_state(&self) -> Option<&str> {
        self.employees
            .iter()
            .find(|e| e.id == "mgr")
            .map(|e| e.state.as_str())
    }
}

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let snapshot = std::env::var("OFFICE_TUI_SNAPSHOT")
        .map(|v| v == "1" || v.eq_ignore_ascii_case("true"))
        .unwrap_or(false)
        || args.iter().any(|a| a == "--snapshot");

    let says = parse_say_args(&args);

    let cfg = Config::from_env();
    // `--say` はヘッドレス専用。指定があれば snapshot と同じ経路で発言してから終了する。
    if snapshot || !says.is_empty() {
        run_snapshot(cfg, &says)
    } else {
        run_tui(cfg)
    }
}

/// 引数から `--say <本文>` を指定順に集める（複数回指定可）。
///
/// `--say=本文` 形式にも対応する。値が続かない `--say`（末尾）は無視する。
fn parse_say_args(args: &[String]) -> Vec<String> {
    let mut says = Vec::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        if let Some(text) = arg.strip_prefix("--say=") {
            says.push(text.to_string());
        } else if arg == "--say" {
            if let Some(text) = iter.next() {
                says.push(text.clone());
            }
        }
    }
    says
}

// ---------------------------------------------------------------------------
// ヘッドレススナップショットモード
// ---------------------------------------------------------------------------

fn run_snapshot(cfg: Config, says: &[String]) -> Result<()> {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;

    let mut app = App::default();
    rt.block_on(async {
        let collect = connect_and_collect(&cfg, &mut app, Duration::from_secs(SNAPSHOT_SECS), says);
        match timeout(Duration::from_secs(SNAPSHOT_SECS + 3), collect).await {
            Ok(Ok(())) => {}
            Ok(Err(err)) => {
                app.status = ConnectionStatus::Disconnected(format!("{err:#}"));
            }
            Err(_) => {
                app.status =
                    ConnectionStatus::Disconnected("接続がタイムアウトしました".to_string());
            }
        }
    });

    print_summary(&cfg, &app);
    Ok(())
}

/// 接続して `hello` を送り、`says` があれば welcome を待ってから順に発言し、
/// `duration` のあいだ受信して `app` を更新する。
async fn connect_and_collect(
    cfg: &Config,
    app: &mut App,
    duration: Duration,
    says: &[String],
) -> Result<()> {
    let (ws, _resp) = tokio_tungstenite::connect_async(cfg.url.as_str()).await?;
    let (mut sink, mut stream) = ws.split();

    let hello = ClientMsg::Hello {
        employee_id: cfg.tui_id.clone(),
        device_id: "tui".to_string(),
        version: cfg.version.clone(),
    };
    sink.send(Message::text(hello.to_json())).await?;
    app.status = ConnectionStatus::Connected;

    // `--say` があるときだけ welcome を待つ。`--snapshot` 単独時の挙動は変えない。
    if !says.is_empty() {
        wait_for_welcome(&mut stream, app).await;
        for text in says {
            let say = ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: text.clone(),
            };
            sink.send(Message::text(say.to_json())).await?;
            // 送信者自身にもサーバーから notice が届くため、ローカルエコーは足さない。
        }
    }

    let deadline = Instant::now() + duration;
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            break;
        }
        match timeout(remaining, stream.next()).await {
            Err(_) => break,   // 収集時間終了
            Ok(None) => break, // 切断
            Ok(Some(Err(_))) => break,
            Ok(Some(Ok(msg))) => {
                if let Some(text) = text_of(&msg)
                    && let Ok(parsed) = ServerMsg::parse(text)
                {
                    app.apply(&parsed);
                }
            }
        }
    }

    let bye = ClientMsg::Bye {
        reason: "snapshot".to_string(),
    };
    let _ = sink.send(Message::text(bye.to_json())).await;
    let _ = sink.close().await;
    Ok(())
}

/// 最初の welcome を短いタイムアウト付きで待つ。届かなくても続行する（発言は試みる）。
async fn wait_for_welcome(stream: &mut WsSource, app: &mut App) {
    let deadline = Instant::now() + WELCOME_TIMEOUT;
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            break;
        }
        match timeout(remaining, stream.next()).await {
            Ok(Some(Ok(msg))) => {
                if let Some(text) = text_of(&msg)
                    && let Ok(parsed) = ServerMsg::parse(text)
                {
                    let is_welcome = matches!(parsed, ServerMsg::Welcome { .. });
                    app.apply(&parsed);
                    if is_welcome {
                        break;
                    }
                }
            }
            // 切断・エラー・タイムアウトでは待たずに先へ進む。
            Ok(Some(Err(_))) | Ok(None) | Err(_) => break,
        }
    }
}

/// スナップショット出力用のタスク行（新しい順、最大5件）。
fn task_summary_lines(tasks: &[TaskInfo]) -> Vec<String> {
    tasks
        .iter()
        .take(5)
        .map(|t| {
            format!(
                "  - status={} assignee={} mode={} title={}",
                t.status, t.assignee, t.mode, t.title
            )
        })
        .collect()
}

fn print_summary(cfg: &Config, app: &App) {
    println!("=== ai-office TUI snapshot ===");
    println!("server: {}", cfg.url);
    println!("observer: {} (device_id=tui)", cfg.tui_id);
    println!("status: {}", app.status.label());

    if app.online.is_empty() {
        println!("online: -");
    } else {
        println!("online: {}", app.online.join(", "));
    }

    if app.employees.is_empty() {
        println!("employees: (社員情報なし)");
    } else {
        println!("employees:");
        for e in &app.employees {
            println!(
                "  - id={} name={} role={} status={} state={}",
                e.id, e.name, e.role, e.status, e.state
            );
        }
    }

    let mgr_balance = app
        .ledger
        .get("mgr")
        .map(|v| v.to_string())
        .unwrap_or_else(|| "-".to_string());
    println!("mgr balance: {mgr_balance}");
    println!("mgr state: {}", app.mgr_state().unwrap_or("-"));

    let rels: Vec<&RelationshipInfo> = app
        .relationships
        .iter()
        .filter(|r| r.from_id == "mgr")
        .collect();
    if rels.is_empty() {
        println!("relationships from mgr: (なし)");
    } else {
        println!("relationships from mgr:");
        for r in rels {
            println!("  - {} {:+}", r.to_id, r.affinity);
        }
    }

    // tasks は新しい順なので先頭から最大5件を「last」として出す。
    let task_lines = task_summary_lines(&app.tasks);
    println!("tasks (last {}):", task_lines.len());
    if task_lines.is_empty() {
        println!("  (なし)");
    } else {
        for line in task_lines {
            println!("{line}");
        }
    }

    let start = app.notices.len().saturating_sub(5);
    let recent = &app.notices[start..];
    println!("notices (last {}):", recent.len());
    if recent.is_empty() {
        println!("  (なし)");
    } else {
        for n in recent {
            println!("  [{}] {}: {} ({})", n.channel, n.from, n.text, n.ts);
        }
    }
}

// ---------------------------------------------------------------------------
// 対話モード
// ---------------------------------------------------------------------------

fn run_tui(cfg: Config) -> Result<()> {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;

    let app = Arc::new(Mutex::new(App::default()));
    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    // UI -> WebSocket の送信チャンネル。切断中は未送信のままバッファされ、再接続後に送られる。
    let (out_tx, out_rx) = mpsc::unbounded_channel::<ClientMsg>();

    // WebSocket 送受信タスク。ランタイムのワーカースレッド上で動く。
    {
        let app = Arc::clone(&app);
        rt.spawn(async move {
            ws_loop(cfg, app, shutdown_rx, out_rx).await;
        });
    }

    // `ratatui::init()` は代替スクリーン + raw モード + panic 時の復元フックを設定する。
    let mut terminal = ratatui::init();
    let result = run_ui(&mut terminal, &app, &out_tx);
    ratatui::restore();

    // 送信タスクに bye を送る余地を与えつつ、確実に片付ける。
    let _ = shutdown_tx.send(true);
    rt.shutdown_timeout(Duration::from_millis(500));

    result
}

/// キー入力をポーリングしながら約10fpsで描画する。
fn run_ui(
    terminal: &mut DefaultTerminal,
    app: &Arc<Mutex<App>>,
    out_tx: &mpsc::UnboundedSender<ClientMsg>,
) -> Result<()> {
    loop {
        let mut viewport = Rect::default();
        {
            let state = app.lock().expect("app mutex poisoned");
            terminal.draw(|frame| {
                viewport = frame.area();
                draw(frame, &state);
            })?;
        }

        if !event::poll(FRAME_INTERVAL)? {
            continue;
        }
        let Event::Key(key) = event::read()? else {
            continue;
        };
        if key.kind != KeyEventKind::Press {
            continue;
        }

        // ページ送りは枠線と入力行を除いた高さで見積もる（最低 1 件）。
        let page = (viewport.height.saturating_sub(CHROME_ROWS) as usize).max(1);
        let mut outgoing: Option<ClientMsg> = None;
        let mut quit = false;
        {
            let mut state = app.lock().expect("app mutex poisoned");
            match key.code {
                // 終了は Esc / Ctrl-C。`q` は入力文字として扱う（§15.2）。
                KeyCode::Esc => quit = true,
                KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => quit = true,
                KeyCode::Enter => {
                    let input = std::mem::take(&mut state.input);
                    let action = parse_input(&input);
                    match action {
                        InputAction::Empty => {}
                        InputAction::Quit => quit = true,
                        InputAction::Help => {
                            for line in HELP_LINES {
                                state.push_local((*line).to_string());
                            }
                        }
                        InputAction::TaskUsage => state.push_local(TASK_USAGE.to_string()),
                        InputAction::Task { title, workspace } => {
                            // サーバーのエコーを待たずに即時フィードバックを出す。
                            if workspace.is_empty() {
                                state.push_local(format!("> /task {title}"));
                            } else {
                                state.push_local(format!("> /task -d {workspace} {title}"));
                            }
                            outgoing = Some(ClientMsg::Task {
                                title,
                                description: String::new(),
                                mode: "local".to_string(),
                                repo: String::new(),
                                base_branch: String::new(),
                                workspace,
                            });
                        }
                        // 発言・回答・スキップの振り分けは純粋関数 [`resolve_outgoing`] に集約し、
                        // ここではその結果（送信メッセージと更新後の保留）を反映するだけにする。
                        InputAction::Say(_)
                        | InputAction::SayForced(_)
                        | InputAction::SkipQuestion => {
                            let is_skip = matches!(&action, InputAction::SkipQuestion);
                            let had_pending = state.pending_question.is_some();
                            let (msg, new_pending) =
                                resolve_outgoing(&action, state.pending_question.as_deref());
                            // `/skip` は保留があった時だけローカル通知を出す（既存挙動を維持）。
                            if is_skip && had_pending {
                                state.push_local("質問への回答をスキップしました".to_string());
                            }
                            state.pending_question = new_pending;
                            outgoing = msg;
                        }
                    }
                }
                // 会議室のスクロール。入力バッファには影響しない。
                KeyCode::PageUp => state.scroll_up(page),
                KeyCode::PageDown => state.scroll_down(page),
                KeyCode::Home => state.scroll_to_oldest(),
                KeyCode::End => state.scroll_to_newest(),
                code => apply_input_key(&mut state.input, code, key.modifiers),
            }
        }

        if quit {
            return Ok(());
        }
        if let Some(msg) = outgoing {
            // 受信側がまだ居ない（起動直後）場合は取りこぼすが、致命的ではない。
            let _ = out_tx.send(msg);
        }
    }
}

/// WebSocket 接続を維持し、切れたら指数バックオフで再接続する。
async fn ws_loop(
    cfg: Config,
    app: Arc<Mutex<App>>,
    mut shutdown: watch::Receiver<bool>,
    mut out_rx: mpsc::UnboundedReceiver<ClientMsg>,
) {
    let mut retries: u64 = 0;
    loop {
        if *shutdown.borrow() {
            return;
        }
        set_status(&app, ConnectionStatus::Connecting);

        match session(&cfg, &app, &mut shutdown, &mut out_rx).await {
            SessionEnd::Shutdown => return,
            SessionEnd::Disconnected(reason) => {
                set_status(&app, ConnectionStatus::Disconnected(reason));
            }
        }

        if *shutdown.borrow() {
            return;
        }
        retries += 1;
        let delay = backoff_delay(retries);
        tokio::select! {
            _ = sleep(delay) => {}
            res = shutdown.changed() => {
                let _ = res;
                if *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

enum SessionEnd {
    Shutdown,
    Disconnected(String),
}

/// 1 セッション分の送受信を多重化する。
///
/// 受信（`stream.next()`）と送信（`out_rx` / heartbeat）を `select!` で同時に進めるので、
/// 受信待ちの間でもユーザー入力や heartbeat を送れる。再接続のたびに呼ばれ、
/// 新しく hello を送り直す（`out_rx` は呼び側が持ち越すため未送信分は失われない）。
async fn session(
    cfg: &Config,
    app: &Arc<Mutex<App>>,
    shutdown: &mut watch::Receiver<bool>,
    out_rx: &mut mpsc::UnboundedReceiver<ClientMsg>,
) -> SessionEnd {
    let ws = match tokio_tungstenite::connect_async(cfg.url.as_str()).await {
        Ok((ws, _resp)) => ws,
        Err(err) => return SessionEnd::Disconnected(format!("{err}")),
    };
    let (mut sink, mut stream) = ws.split();

    let hello = ClientMsg::Hello {
        employee_id: cfg.tui_id.clone(),
        device_id: "tui".to_string(),
        version: cfg.version.clone(),
    };
    if let Err(err) = sink.send(Message::text(hello.to_json())).await {
        return SessionEnd::Disconnected(format!("hello send failed: {err}"));
    }
    set_status(app, ConnectionStatus::Connected);

    // hello 直後に tick が飛ばないよう、最初の 1 回は間隔後から始める。
    let mut heartbeat = interval_at(Instant::now() + HEARTBEAT_INTERVAL, HEARTBEAT_INTERVAL);

    loop {
        tokio::select! {
            res = shutdown.changed() => {
                if res.is_err() || *shutdown.borrow() {
                    let _ = send_bye(&mut sink).await;
                    let _ = sink.close().await;
                    return SessionEnd::Shutdown;
                }
            }
            incoming = stream.next() => {
                match incoming {
                    Some(Ok(msg)) => {
                        if let Some(text) = text_of(&msg)
                            && let Ok(parsed) = ServerMsg::parse(text)
                        {
                            app.lock().expect("app mutex poisoned").apply(&parsed);
                        }
                    }
                    Some(Err(err)) => return SessionEnd::Disconnected(err.to_string()),
                    None => return SessionEnd::Disconnected(String::new()),
                }
            }
            outgoing = out_rx.recv() => {
                match outgoing {
                    Some(msg) => {
                        if let Err(err) = sink.send(Message::text(msg.to_json())).await {
                            return SessionEnd::Disconnected(format!("send failed: {err}"));
                        }
                    }
                    // UI 側の送信チャンネルが閉じた = TUI 終了。
                    None => {
                        let _ = send_bye(&mut sink).await;
                        let _ = sink.close().await;
                        return SessionEnd::Shutdown;
                    }
                }
            }
            _ = heartbeat.tick() => {
                let hb = ClientMsg::Heartbeat { ts: now_iso() };
                if let Err(err) = sink.send(Message::text(hb.to_json())).await {
                    return SessionEnd::Disconnected(format!("heartbeat failed: {err}"));
                }
            }
        }
    }
}

async fn send_bye(sink: &mut WsSink) -> Result<()> {
    let bye = ClientMsg::Bye {
        reason: "shutdown".to_string(),
    };
    sink.send(Message::text(bye.to_json())).await?;
    Ok(())
}

fn set_status(app: &Arc<Mutex<App>>, status: ConnectionStatus) {
    app.lock().expect("app mutex poisoned").status = status;
}

// ---------------------------------------------------------------------------
// 描画
// ---------------------------------------------------------------------------

fn draw(frame: &mut Frame, app: &App) {
    let chunks = Layout::vertical([Constraint::Min(1), Constraint::Length(1)]).split(frame.area());
    let body = chunks[0];

    // 幅に応じて出すペインを減らす。狭い端末で空の枠を残さない。
    if body.width >= THREE_PANE_MIN_WIDTH {
        let panes = Layout::horizontal([
            Constraint::Min(LEFT_MIN_WIDTH),
            Constraint::Percentage(44),
            Constraint::Min(RIGHT_MIN_WIDTH),
        ])
        .split(body);
        draw_left_column(frame, panes[0], app);
        draw_notices(frame, panes[1], app);
        draw_mgr(frame, panes[2], app);
    } else if body.width >= TWO_PANE_MIN_WIDTH {
        let panes = Layout::horizontal([
            Constraint::Min(LEFT_MIN_WIDTH),
            Constraint::Min(CENTER_MIN_WIDTH),
        ])
        .split(body);
        draw_left_column(frame, panes[0], app);
        draw_notices(frame, panes[1], app);
    } else {
        // 極端に狭いときは #会議室 だけを全幅で見せる。
        draw_notices(frame, body, app);
    }
    draw_input(frame, chunks[1], app);
}

/// 左列を上下に分割する（社員 上 / タスク 下）。両方に最低数行を確保する。
fn draw_left_column(frame: &mut Frame, area: Rect, app: &App) {
    let rows = Layout::vertical([Constraint::Min(3), Constraint::Min(2)]).split(area);
    draw_employees(frame, rows[0], app);
    draw_tasks(frame, rows[1], app);
}

fn draw_employees(frame: &mut Frame, area: Rect, app: &App) {
    let items: Vec<ListItem> = app
        .employee_rows()
        .iter()
        .map(|e| {
            let (icon, label, color) = status_display(&e.status);
            // 表示は名前を優先。名前が無ければ id で代用する。
            let name = if e.name.is_empty() { &e.id } else { &e.name };
            let line = Line::from(vec![
                Span::styled(icon, Style::default().fg(color)),
                Span::raw(" "),
                Span::raw(name.clone()),
                Span::raw(" "),
                Span::styled(label, Style::default().fg(Color::DarkGray)),
            ]);
            ListItem::new(line)
        })
        .collect();

    let list = List::new(items).block(Block::bordered().title(Line::styled(
        "社員",
        Style::default().add_modifier(Modifier::BOLD),
    )));
    frame.render_widget(list, area);
}

fn draw_tasks(frame: &mut Frame, area: Rect, app: &App) {
    let block = Block::bordered().title(Line::styled(
        "タスク",
        Style::default().add_modifier(Modifier::BOLD),
    ));
    let inner = block.inner(area);
    // 枠線の内側の表示幅（全角は幅2で数える）。
    let max_width = inner.width as usize;

    let items: Vec<ListItem> = if app.tasks.is_empty() {
        vec![ListItem::new(Line::from(Span::styled(
            "(なし)",
            Style::default().fg(Color::DarkGray),
        )))]
    } else {
        app.tasks
            .iter()
            .take(MAX_TASK_ROWS)
            .map(|t| {
                let (prefix, color) = task_status_prefix(&t.status);
                let label = if t.status.is_empty() {
                    "不明"
                } else {
                    t.status.as_str()
                };
                // 例: "✔done レポート作成 (dev_m)"。ペイン幅に収まるよう切り詰める。
                let text = format!("{prefix}{label} {} ({})", t.title, t.assignee);
                let text = truncate_to_width(&text, max_width);
                ListItem::new(Line::from(Span::styled(text, Style::default().fg(color))))
            })
            .collect()
    };

    frame.render_widget(List::new(items).block(block), area);
}

fn draw_notices(frame: &mut Frame, area: Rect, app: &App) {
    let scroll = app.scroll;
    let title = if scroll == 0 {
        Line::styled(
            DEFAULT_CHANNEL,
            Style::default().add_modifier(Modifier::BOLD),
        )
    } else {
        Line::from(vec![
            Span::styled(
                DEFAULT_CHANNEL,
                Style::default().add_modifier(Modifier::BOLD),
            ),
            Span::styled(
                format!("  ▲ 上に {scroll} 件 (End で最新)"),
                Style::default().fg(Color::Yellow),
            ),
        ])
    };
    let block = Block::bordered().title(title);
    let inner = block.inner(area);
    let width = inner.width as usize;
    let height = inner.height as usize;

    // スクロール位置の分だけ新しい側を切り離し、内側幅で折り返して描く。
    // 位置は「下（最新）からの件数」なので、新着が来ても相対位置は変わらない。
    let end = app.notices.len().saturating_sub(scroll);
    let mut lines: Vec<Line> = Vec::new();
    for notice in &app.notices[..end] {
        lines.extend(notice_lines(notice, width));
    }

    // 折り返し後の行数で最下部に合わせる（0 件・高さ 0 でも saturating_sub で安全）。
    let total = lines.len();
    let offset = total.saturating_sub(height);
    let offset = offset.min(u16::MAX as usize) as u16;

    let paragraph = Paragraph::new(lines).block(block).scroll((offset, 0));
    frame.render_widget(paragraph, area);
}

/// notice 1 件を、内側幅 `width` で折り返した行へ変換する。
fn notice_lines(notice: &NoticeLine, width: usize) -> Vec<Line<'static>> {
    if notice.local {
        // 送信エコー / ヘルプ / システム行は一目で区別できるよう色を変える。
        wrap_styled(
            &[(notice.text.clone(), Style::default().fg(Color::Cyan))],
            width,
        )
    } else {
        // 送信者は太字、本文は通常。
        wrap_styled(
            &[
                (
                    format!("{}: ", notice.from),
                    Style::default().add_modifier(Modifier::BOLD),
                ),
                (notice.text.clone(), Style::default()),
            ],
            width,
        )
    }
}

/// スタイル付きのテキスト片を表示幅 `width` で折り返して行に変換する。
///
/// 改行 `\n` は強制改行として扱う。単語境界は考慮せず表示幅（全角は 2）で
/// 分割する。幅 0 でも 1 行は返し、呼び側が空表示を安全に扱えるようにする。
fn wrap_styled(parts: &[(String, Style)], width: usize) -> Vec<Line<'static>> {
    let max = width.max(1);
    let mut lines: Vec<Line<'static>> = Vec::new();
    let mut spans: Vec<Span<'static>> = Vec::new();
    let mut w = 0usize;

    for (text, style) in parts {
        let mut buf = String::new();
        for ch in text.chars() {
            if ch == '\n' {
                if !buf.is_empty() {
                    spans.push(Span::styled(std::mem::take(&mut buf), *style));
                }
                lines.push(Line::from(std::mem::take(&mut spans)));
                w = 0;
                continue;
            }
            let cw = ch.width().unwrap_or(0);
            // 幅を超える前に改行する（行頭の 1 文字は必ず置く）。
            if w + cw > max && w > 0 {
                if !buf.is_empty() {
                    spans.push(Span::styled(std::mem::take(&mut buf), *style));
                }
                lines.push(Line::from(std::mem::take(&mut spans)));
                w = 0;
            }
            buf.push(ch);
            w += cw;
        }
        if !buf.is_empty() {
            spans.push(Span::styled(std::mem::take(&mut buf), *style));
        }
    }

    lines.push(Line::from(spans));
    lines
}

fn draw_mgr(frame: &mut Frame, area: Rect, app: &App) {
    let balance = app
        .ledger
        .get("mgr")
        .map(|v| v.to_string())
        .unwrap_or_else(|| "-".to_string());
    let state = app.mgr_state().unwrap_or("-").to_string();

    let mut lines = vec![
        Line::from(vec![
            Span::raw("学: "),
            Span::styled(balance, Style::default().fg(Color::Yellow)),
        ]),
        Line::from(vec![Span::raw("状態: "), Span::raw(state)]),
        Line::from("関係:"),
    ];

    let rels: Vec<&RelationshipInfo> = app
        .relationships
        .iter()
        .filter(|r| r.from_id == "mgr")
        .collect();
    if rels.is_empty() {
        lines.push(Line::from(Span::styled(
            "  (なし)",
            Style::default().fg(Color::DarkGray),
        )));
    } else {
        for r in rels {
            let color = if r.affinity >= 0 {
                Color::Green
            } else {
                Color::Red
            };
            lines.push(Line::from(vec![
                Span::raw("  "),
                Span::raw(format!("{} ", r.to_id)),
                Span::styled(format!("{:+}", r.affinity), Style::default().fg(color)),
            ]));
        }
    }

    let paragraph = Paragraph::new(lines).block(Block::bordered().title(Line::styled(
        "mgr",
        Style::default().add_modifier(Modifier::BOLD),
    )));
    frame.render_widget(paragraph, area);
}

fn draw_input(frame: &mut Frame, area: Rect, app: &App) {
    let status_text = app.status.label();
    let status_width = (UnicodeWidthStr::width(status_text.as_str()) as u16)
        .min(MAX_STATUS_WIDTH)
        .min(area.width.saturating_sub(1));
    // ヒントは上限を設け、入力欄を最低限確保してから残り幅をあてる。
    let hint_width = (UnicodeWidthStr::width(INPUT_HINT) as u16)
        .min(HINT_MAX_WIDTH)
        .min(
            area.width
                .saturating_sub(status_width)
                .saturating_sub(INPUT_MIN_WIDTH),
        );

    let cols = Layout::horizontal([
        Constraint::Min(1),
        Constraint::Length(hint_width),
        Constraint::Length(status_width),
    ])
    .split(area);

    // 入力行（カーソルは常に末尾なので、空でなければカーソル記号を添える）。
    // 保留中の質問がある間は、通常入力が回答になることを分かりやすく示す。
    let prompt = if app.pending_question.is_some() {
        "回答> "
    } else {
        "> "
    };
    let mut spans = vec![Span::styled(
        prompt,
        Style::default().add_modifier(Modifier::BOLD),
    )];
    if app.input.is_empty() {
        spans.push(Span::styled("_", Style::default().fg(Color::DarkGray)));
    } else {
        spans.push(Span::raw(app.input.clone()));
        spans.push(Span::styled("‸", Style::default().fg(Color::Gray)));
    }
    frame.render_widget(Paragraph::new(Line::from(spans)), cols[0]);

    // ヒント（控えめな色）。
    let hint = truncate_to_width(INPUT_HINT, hint_width as usize);
    frame.render_widget(
        Paragraph::new(Span::styled(hint, Style::default().fg(Color::DarkGray))),
        cols[1],
    );

    // 接続状態（色付き）。
    let status = truncate_to_width(&status_text, status_width as usize);
    frame.render_widget(
        Paragraph::new(Span::styled(
            status,
            Style::default().fg(app.status.color()),
        ))
        .alignment(ratatui::layout::Alignment::Right),
        cols[2],
    );
}

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

/// Enter で確定した入力の解釈結果。
#[derive(Debug, Clone, PartialEq, Eq)]
enum InputAction {
    /// 空入力。何もしない。
    Empty,
    /// 終了。
    Quit,
    /// ヘルプ表示。
    Help,
    /// タスク投入（タイトルと任意の作業先）。
    Task {
        title: String,
        /// `-d <dir>` / `--dir <dir>` で指定した作業先。未指定なら空文字。
        workspace: String,
    },
    /// タイトル無しの `/task`。
    TaskUsage,
    /// #会議室 への発言（保留中の質問があれば回答として扱う）。
    Say(String),
    /// `/say <本文>`。保留中の質問があっても #会議室 へ発言する（強制）。
    SayForced(String),
    /// `/skip`・`/cancel`。保留中の質問への回答をやめる。
    SkipQuestion,
}

/// 入力行の文字列を解釈する（§15.2）。
///
/// - `/task [-d <作業先>] <タイトル>` → [`InputAction::Task`]（タイトル必須）
/// - `/help` / `/quit` / `/exit`
/// - `/say <本文>` → [`InputAction::SayForced`]（保留中の質問があっても発言）
/// - `/skip` / `/cancel` → [`InputAction::SkipQuestion`]
/// - それ以外の非空入力 → [`InputAction::Say`]
/// - 空白のみ → [`InputAction::Empty`]
///
/// 保留中の質問の有無で `Say` を回答に振り分けるのは [`resolve_outgoing`] で行う。
/// この関数は純粋にし、テストしやすさを保つ。
fn parse_input(input: &str) -> InputAction {
    let input = input.trim();
    if input.is_empty() {
        return InputAction::Empty;
    }
    match input {
        "/quit" | "/exit" => return InputAction::Quit,
        "/help" => return InputAction::Help,
        "/task" => return InputAction::TaskUsage,
        "/skip" | "/cancel" => return InputAction::SkipQuestion,
        // `/say` 単体は本文が無いので何もしない（文字列 "/say" を発言にしない）。
        "/say" => return InputAction::Empty,
        _ => {}
    }
    if let Some(rest) = input.strip_prefix("/say") {
        let mut chars = rest.chars();
        match chars.next() {
            None => return InputAction::Empty,
            // `/say <本文>`（空白区切り）だけをコマンドとして扱う。
            Some(c) if c.is_whitespace() => {
                let text = chars.as_str().trim();
                if text.is_empty() {
                    return InputAction::Empty;
                }
                return InputAction::SayForced(text.to_string());
            }
            // `/sayfoo` のような未知の語は発言として扱う。
            Some(_) => {}
        }
    }
    if let Some(rest) = input.strip_prefix("/task") {
        let mut chars = rest.chars();
        match chars.next() {
            // `/task` 単体は上で処理済みだが、念のため。
            None => return InputAction::TaskUsage,
            // `/task ...`（空白区切り）だけをコマンドとして扱う。
            Some(c) if c.is_whitespace() => {
                let rest = chars.as_str().trim();
                if rest.is_empty() {
                    return InputAction::TaskUsage;
                }
                return parse_task_args(rest);
            }
            // `/taskfoo` のような未知の語は発言として扱う。
            Some(_) => {}
        }
    }
    InputAction::Say(input.to_string())
}

/// `/task` の引数を解釈する。
///
/// `-d <dir>` / `--dir <dir>` を先頭に付けると作業先を指定できる（省略可）。
/// 例: `/task -d /home/user/Dev/site 天気サイトを作る`
/// `-d` に値が無い、または `-d` の後にタイトルが無い場合は使い方表示にする。
fn parse_task_args(rest: &str) -> InputAction {
    let mut tokens = rest.split_whitespace();
    match tokens.next() {
        Some("-d") | Some("--dir") => {
            let Some(workspace) = tokens.next() else {
                return InputAction::TaskUsage;
            };
            let title = tokens.collect::<Vec<_>>().join(" ");
            if title.is_empty() {
                return InputAction::TaskUsage;
            }
            InputAction::Task {
                title,
                workspace: workspace.to_string(),
            }
        }
        // `-d` 無しはタイトル全体（空白はそのまま保持）。
        _ => InputAction::Task {
            title: rest.to_string(),
            workspace: String::new(),
        },
    }
}

/// 入力アクションと保留中の質問から、送信するメッセージと更新後の保留を決める純粋関数。
///
/// - 保留中でも `@宛先` や `/コマンド` で始まる [`InputAction::Say`] は通常会議室発言として
///   送る（回答に飲み込まれると「chat しか応答しない」ように見えるため）。保留はそのまま残す。
/// - それ以外で保留中の質問があれば [`ClientMsg::Answer`] を送り、保留をクリアする。
/// - [`InputAction::SayForced`]（`/say`）は保留に関係なく必ず会議室発言する（保留は残す）。
/// - [`InputAction::SkipQuestion`]（`/skip`・`/cancel`）は送信せず保留だけクリアする。
/// - それ以外のアクション（`Empty`/`Quit`/`Help`/`Task`/`TaskUsage`）はここでは何も決めない
///   （メッセージを送らず保留も変えない）。副作用系は呼び側（`run_ui`）に残す。
fn resolve_outgoing(
    action: &InputAction,
    pending: Option<&str>,
) -> (Option<ClientMsg>, Option<String>) {
    match action {
        InputAction::Say(text) => {
            let is_mention_or_cmd = text.starts_with('@') || text.starts_with('/');
            match pending {
                Some(question_id) if !is_mention_or_cmd => (
                    Some(ClientMsg::Answer {
                        question_id: question_id.to_string(),
                        text: text.clone(),
                    }),
                    None,
                ),
                // 通常会議室発言。送信者自身にもサーバーから notice が届くのでローカルエコーは出さない。
                _ => (
                    Some(ClientMsg::Say {
                        channel: DEFAULT_CHANNEL.to_string(),
                        text: text.clone(),
                    }),
                    pending.map(str::to_string),
                ),
            }
        }
        // `/say` は保留中の質問があっても #会議室 へ発言する。
        InputAction::SayForced(text) => (
            Some(ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: text.clone(),
            }),
            pending.map(str::to_string),
        ),
        // `/skip`・`/cancel` は回答せずに保留をクリアする（送信しない）。通知は呼び側。
        InputAction::SkipQuestion => (None, None),
        // 上記以外（`Empty`/`Quit`/`Help`/`Task`/`TaskUsage`）はここでは何もしない。
        _ => (None, pending.map(str::to_string)),
    }
}

/// 入力バッファに 1 キー分の編集を適用する（Enter や終了系は呼び側で処理する）。
fn apply_input_key(input: &mut String, code: KeyCode, modifiers: KeyModifiers) {
    match code {
        KeyCode::Backspace => {
            input.pop();
        }
        KeyCode::Char('u') if modifiers.contains(KeyModifiers::CONTROL) => input.clear(),
        KeyCode::Char(c) if !modifiers.contains(KeyModifiers::CONTROL) => input.push(c),
        _ => {}
    }
}

/// 現在時刻を RFC 3339（秒精度, UTC, `Z`）で返す（heartbeat 用）。
fn now_iso() -> String {
    chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
}

/// status から（アイコン, 日本語ラベル, 色）を返す。
fn status_display(status: &str) -> (&'static str, String, Color) {
    match status {
        "online" => ("●", "在席".to_string(), Color::Green),
        "offline" => ("○", "退勤".to_string(), Color::DarkGray),
        "busy" => ("▲", "取り込み中".to_string(), Color::Yellow),
        "break" => ("▲", "休憩".to_string(), Color::Yellow),
        "" => ("▲", "不明".to_string(), Color::Yellow),
        other => ("▲", other.to_string(), Color::Yellow),
    }
}

/// タスクの status から（プレフィックス, 色）を返す（§13.3 の状態遷移を想定）。
fn task_status_prefix(status: &str) -> (&'static str, Color) {
    match status {
        "done" => ("✔", Color::Green),
        "working" => ("▶", Color::Yellow),
        "review" => ("◀", Color::Yellow),
        // pending / assigned は既定色（強調しない）。
        "assigned" => ("○", Color::Reset),
        "pending" => ("·", Color::Reset),
        "failed" => ("✗", Color::Red),
        _ => ("·", Color::DarkGray),
    }
}

/// タスク行をペインの表示幅に合わせて切り詰める。切ったときは末尾に `…` を付ける。
fn truncate_to_width(s: &str, max: usize) -> String {
    if s.width() <= max {
        return s.to_string();
    }
    if max == 0 {
        return String::new();
    }
    let limit = max - 1; // `…` の分を確保する。
    let mut out = String::new();
    let mut w = 0usize;
    for ch in s.chars() {
        let cw = ch.width().unwrap_or(0);
        if w + cw > limit {
            break;
        }
        out.push(ch);
        w += cw;
    }
    out.push('…');
    out
}

fn text_of(msg: &Message) -> Option<&str> {
    match msg {
        Message::Text(text) => Some(text.as_str()),
        _ => None,
    }
}

/// 指数バックオフ（1s, 2s, 4s, ... 上限 [`BACKOFF_MAX_SECS`] 秒）。
fn backoff_delay(attempt: u64) -> Duration {
    let exp = attempt.saturating_sub(1).min(16);
    let secs = BACKOFF_BASE_SECS
        .saturating_mul(1u64 << exp)
        .min(BACKOFF_MAX_SECS);
    Duration::from_secs(secs)
}

fn env_string(key: &str, default: &str) -> String {
    std::env::var(key)
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| default.to_string())
}

#[cfg(test)]
mod tests {
    use super::{
        App, ConnectionStatus, DEFAULT_CHANNEL, DEFAULT_TUI_ID, HELP_LINES, InputAction,
        apply_input_key, backoff_delay, parse_input, parse_say_args, resolve_outgoing,
        status_display, task_status_prefix, task_summary_lines, truncate_to_width,
    };
    use crossterm::event::{KeyCode, KeyModifiers};
    use protocol::{ClientMsg, EmployeeInfo, Office, ServerMsg, TaskInfo};
    use std::collections::BTreeMap;
    use std::time::Duration;

    /// テスト用のタスクを組み立てる。
    fn task(id: &str, title: &str, status: &str, assignee: &str) -> TaskInfo {
        TaskInfo {
            id: id.to_string(),
            title: title.to_string(),
            status: status.to_string(),
            assignee: assignee.to_string(),
            mode: "local".to_string(),
            result: String::new(),
        }
    }

    /// テスト用の welcome を組み立てる。
    fn welcome(online: &[&str]) -> ServerMsg {
        ServerMsg::Welcome {
            session_id: "s".to_string(),
            server_time: "t".to_string(),
            office: Office {
                online: online.iter().map(|s| (*s).to_string()).collect(),
            },
        }
    }

    /// テスト用の notice を組み立てる。
    fn notice(from: &str, text: &str) -> ServerMsg {
        ServerMsg::Notice {
            channel: DEFAULT_CHANNEL.to_string(),
            from: from.to_string(),
            text: text.to_string(),
            ts: "2026-09-27T14:12:01Z".to_string(),
        }
    }

    #[test]
    fn task_summary_lines_format_matches_snapshot_spec() {
        let tasks = vec![task("t1", "A", "working", "dev_m")];
        assert_eq!(
            task_summary_lines(&tasks),
            vec!["  - status=working assignee=dev_m mode=local title=A"]
        );
        // 空なら行なし（呼び側で (なし) を出す）。
        assert!(task_summary_lines(&[]).is_empty());
    }

    #[test]
    fn backoff_is_exponential_with_cap() {
        assert_eq!(backoff_delay(1), Duration::from_secs(1));
        assert_eq!(backoff_delay(2), Duration::from_secs(2));
        assert_eq!(backoff_delay(3), Duration::from_secs(4));
        assert_eq!(backoff_delay(4), Duration::from_secs(8));
        assert_eq!(backoff_delay(5), Duration::from_secs(16));
        assert_eq!(backoff_delay(6), Duration::from_secs(30));
        assert_eq!(backoff_delay(99), Duration::from_secs(30));
    }

    #[test]
    fn parse_input_commands_and_say() {
        assert_eq!(parse_input(""), InputAction::Empty);
        assert_eq!(parse_input("    "), InputAction::Empty);
        assert_eq!(
            parse_input("/task x"),
            InputAction::Task {
                title: "x".to_string(),
                workspace: String::new(),
            }
        );
        assert_eq!(
            parse_input("/task   レポート作成"),
            InputAction::Task {
                title: "レポート作成".to_string(),
                workspace: String::new(),
            }
        );
        assert_eq!(parse_input("/task"), InputAction::TaskUsage);
        assert_eq!(parse_input("/task   "), InputAction::TaskUsage);
        assert_eq!(parse_input("/help"), InputAction::Help);
        assert_eq!(parse_input("/quit"), InputAction::Quit);
        assert_eq!(parse_input("/exit"), InputAction::Quit);
        // コマンド以外の非空入力は発言。前後の空白は落とす。
        assert_eq!(
            parse_input("おはよう"),
            InputAction::Say("おはよう".to_string())
        );
        assert_eq!(
            parse_input("  hello world  "),
            InputAction::Say("hello world".to_string())
        );
        // `/taskfoo` はコマンドではなく発言として扱う。
        assert_eq!(
            parse_input("/taskfoo"),
            InputAction::Say("/taskfoo".to_string())
        );
    }

    #[test]
    fn parse_task_with_dir_flag() {
        // `-d <dir>` で作業先を指定でき、title には残りが入る。
        assert_eq!(
            parse_input("/task -d /home/user/Dev/site 天気サイトを作る"),
            InputAction::Task {
                title: "天気サイトを作る".to_string(),
                workspace: "/home/user/Dev/site".to_string(),
            }
        );
        // `--dir` 长音も受け付ける。
        assert_eq!(
            parse_input("/task --dir /tmp/x レポート"),
            InputAction::Task {
                title: "レポート".to_string(),
                workspace: "/tmp/x".to_string(),
            }
        );
        // `-d` の値が無い、またはタイトルが無い場合は使い方表示。
        assert_eq!(parse_input("/task -d"), InputAction::TaskUsage);
        assert_eq!(parse_input("/task --dir"), InputAction::TaskUsage);
        assert_eq!(parse_input("/task -d /tmp/x"), InputAction::TaskUsage);
        // `-d` 無しは従来どおり workspace は空。
        assert_eq!(
            parse_input("/task レポート"),
            InputAction::Task {
                title: "レポート".to_string(),
                workspace: String::new(),
            }
        );
    }

    #[test]
    fn parse_say_args_collects_repeated_flags() {
        let args: Vec<String> = [
            "tui",
            "--say",
            "@mgr 点呼",
            "--say=おはよう",
            "--snapshot",
            "--say",
            "終わり",
        ]
        .iter()
        .map(|s| (*s).to_string())
        .collect();
        // 指定順に集め、`--say=本文` 形式も受け付ける。
        assert_eq!(
            parse_say_args(&args),
            vec![
                "@mgr 点呼".to_string(),
                "おはよう".to_string(),
                "終わり".to_string(),
            ]
        );
    }

    #[test]
    fn parse_say_args_ignores_missing_value_and_absent_flag() {
        let to_args =
            |xs: &[&str]| -> Vec<String> { xs.iter().map(|s| (*s).to_string()).collect() };
        // `--say` が末尾で値が無いときは無視する。
        assert!(parse_say_args(&to_args(&["tui", "--say"])).is_empty());
        // `--say` 未指定なら空。
        assert!(parse_say_args(&to_args(&["tui", "--snapshot"])).is_empty());
        assert!(parse_say_args(&[]).is_empty());
    }

    #[test]
    fn input_editing_insert_backspace_clear() {
        let mut buf = String::new();
        apply_input_key(&mut buf, KeyCode::Char('a'), KeyModifiers::NONE);
        apply_input_key(&mut buf, KeyCode::Char('b'), KeyModifiers::NONE);
        apply_input_key(&mut buf, KeyCode::Char('c'), KeyModifiers::NONE);
        assert_eq!(buf, "abc");

        apply_input_key(&mut buf, KeyCode::Backspace, KeyModifiers::NONE);
        assert_eq!(buf, "ab");
        // 空で Backspace しても panic しない。
        for _ in 0..5 {
            apply_input_key(&mut buf, KeyCode::Backspace, KeyModifiers::NONE);
        }
        assert_eq!(buf, "");

        // Ctrl-U でクリア。
        apply_input_key(&mut buf, KeyCode::Char('x'), KeyModifiers::NONE);
        apply_input_key(&mut buf, KeyCode::Char('y'), KeyModifiers::NONE);
        apply_input_key(&mut buf, KeyCode::Char('u'), KeyModifiers::CONTROL);
        assert_eq!(buf, "");

        // 制御文字は挿入しない。
        apply_input_key(&mut buf, KeyCode::Char('z'), KeyModifiers::CONTROL);
        assert_eq!(buf, "");

        // `q` は入力文字（終了ではない）。
        apply_input_key(&mut buf, KeyCode::Char('q'), KeyModifiers::NONE);
        assert_eq!(buf, "q");
    }

    #[test]
    fn status_icons_match_spec() {
        assert_eq!(status_display("online").0, "●");
        assert_eq!(status_display("offline").0, "○");
        assert_eq!(status_display("busy").0, "▲");
        assert_eq!(status_display("break").0, "▲");
        assert_eq!(status_display("weird").0, "▲");
    }

    #[test]
    fn apply_office_state_updates_everything() {
        let mut app = App::default();
        let msg = ServerMsg::OfficeState {
            online: vec!["mgr".to_string()],
            employees: vec![EmployeeInfo {
                id: "mgr".to_string(),
                name: "マネージャー".to_string(),
                role: "manager".to_string(),
                status: "online".to_string(),
                state: "idle".to_string(),
            }],
            ledger: BTreeMap::from([("mgr".to_string(), 512i64)]),
            relationships: vec![],
            tasks: vec![task("t1", "レポート作成", "working", "dev_m")],
            ts: "t".to_string(),
        };
        app.apply(&msg);
        assert_eq!(app.online, vec!["mgr"]);
        assert_eq!(app.ledger.get("mgr"), Some(&512));
        assert_eq!(app.mgr_state(), Some("idle"));
        assert_eq!(app.tasks.len(), 1);
        assert_eq!(app.tasks[0].id, "t1");
    }

    #[test]
    fn apply_office_state_replaces_tasks_not_appends() {
        let mut app = App::default();
        app.apply(&ServerMsg::OfficeState {
            online: vec![],
            employees: vec![],
            ledger: BTreeMap::new(),
            relationships: vec![],
            tasks: vec![task("a", "A", "working", "dev_m")],
            ts: "t1".to_string(),
        });
        assert_eq!(app.tasks.len(), 1);

        // 次のスナップショットは追記ではなく置換（新しいスナップショットが正）。
        app.apply(&ServerMsg::OfficeState {
            online: vec![],
            employees: vec![],
            ledger: BTreeMap::new(),
            relationships: vec![],
            tasks: vec![
                task("b", "B", "done", "dev_m"),
                task("c", "C", "pending", "dev_f"),
            ],
            ts: "t2".to_string(),
        });
        assert_eq!(app.tasks.len(), 2);
        assert_eq!(app.tasks[0].id, "b");
        assert_eq!(app.tasks[1].id, "c");

        // 空のスナップショットでクリアされる。
        app.apply(&ServerMsg::OfficeState {
            online: vec![],
            employees: vec![],
            ledger: BTreeMap::new(),
            relationships: vec![],
            tasks: vec![],
            ts: "t3".to_string(),
        });
        assert!(app.tasks.is_empty());
    }

    #[test]
    fn welcome_clears_history_and_scroll_before_replay() {
        let mut app = App::default();
        for i in 0..5 {
            app.push_local(format!("m{i}"));
        }
        app.scroll_up(3);
        assert_eq!(app.notices.len(), 5);
        assert_eq!(app.scroll, 3);

        // 再接続では welcome のあとに snapshot と直近ログが続けて届く。
        // welcome の時点で履歴とスクロールを捨てることで、同じ会話が重複しない。
        app.apply(&welcome(&["mgr"]));
        assert!(app.notices.is_empty());
        assert_eq!(app.scroll, 0);
        assert_eq!(app.online, vec!["mgr"]);
    }

    #[test]
    fn reconnect_replaces_history_without_duplication() {
        let mut app = App::default();
        // 1 回目の接続: welcome のあとに履歴が 2 件届く。
        app.apply(&welcome(&[]));
        app.apply(&notice(DEFAULT_TUI_ID, "a"));
        app.apply(&notice("mgr", "b"));
        assert_eq!(app.notices.len(), 2);

        // 2 回目の接続: サーバーは同じ履歴（+ 1 件）を再送する。
        // welcome でクリアしてから受け直すので、重複せず 3 件になる。
        app.apply(&welcome(&[]));
        app.apply(&notice(DEFAULT_TUI_ID, "a"));
        app.apply(&notice("mgr", "b"));
        app.apply(&notice("mgr", "c"));
        assert_eq!(app.notices.len(), 3);
        assert_eq!(app.notices[0].text, "a");
        assert_eq!(app.notices[2].text, "c");
    }

    #[test]
    fn own_notice_is_shown_once_without_local_echo() {
        // 送信側のローカルエコーは廃止し、送信者自身にも配られるサーバー notice を
        // そのまま表示する。よって自分（tui_id）の notice 1 通は 1 行だけになる。
        let mut app = App::default();
        app.apply(&notice(DEFAULT_TUI_ID, "こんにちは"));
        assert_eq!(app.notices.len(), 1, "自分の発言が二重にならないこと");
        assert_eq!(app.notices[0].from, DEFAULT_TUI_ID);
        assert_eq!(app.notices[0].text, "こんにちは");
        // サーバー由来の行として描画する（ローカル行ではない）。
        assert!(!app.notices[0].local);
    }

    #[test]
    fn task_status_prefixes_match_spec() {
        assert_eq!(task_status_prefix("done").0, "✔");
        assert_eq!(task_status_prefix("working").0, "▶");
        assert_eq!(task_status_prefix("review").0, "◀");
        assert_eq!(task_status_prefix("assigned").0, "○");
        assert_eq!(task_status_prefix("pending").0, "·");
        assert_eq!(task_status_prefix("failed").0, "✗");
        assert_eq!(task_status_prefix("weird").0, "·");
    }

    #[test]
    fn truncate_to_width_respects_display_width() {
        assert_eq!(truncate_to_width("abcdef", 6), "abcdef");
        assert_eq!(truncate_to_width("abcdef", 10), "abcdef");
        assert_eq!(truncate_to_width("abcdef", 4), "abc…");
        assert_eq!(truncate_to_width("abcdef", 0), "");
        // 全角は幅2として数える。"レポート" は幅 8。
        assert_eq!(truncate_to_width("レポート", 8), "レポート");
        assert_eq!(truncate_to_width("レポート", 6), "レポ…");
        assert_eq!(truncate_to_width("レポート", 1), "…");
    }

    #[test]
    fn employees_synthesised_from_online_when_missing() {
        let mut app = App::default();
        app.apply(&ServerMsg::Welcome {
            session_id: "s".to_string(),
            server_time: "t".to_string(),
            office: protocol::Office {
                online: vec!["mgr".to_string(), "dev_m".to_string()],
            },
        });
        let rows = app.employee_rows();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].id, "mgr");
        assert_eq!(rows[0].status, "online");
    }

    #[test]
    fn disconnected_label_is_japanese() {
        let label = ConnectionStatus::Disconnected(String::new()).label();
        assert!(label.contains("接続が切れました"));
    }

    /// テスト用に TUI を描画してプレーンテキスト化する。
    fn render_to_string(app: &App, width: u16, height: u16) -> String {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;

        let mut terminal = Terminal::new(TestBackend::new(width, height)).unwrap();
        terminal.draw(|frame| super::draw(frame, app)).unwrap();

        let buf = terminal.backend().buffer();
        let mut view = String::new();
        for y in 0..buf.area.height {
            for x in 0..buf.area.width {
                view.push_str(buf.cell((x, y)).map(|c| c.symbol()).unwrap_or(" "));
            }
            view.push('\n');
        }
        view
    }

    #[test]
    fn draw_renders_panes_without_panicking() {
        let mut app = App::default();
        app.apply(&ServerMsg::OfficeState {
            online: vec!["mgr".to_string(), "dev_m".to_string()],
            employees: vec![
                EmployeeInfo {
                    id: "mgr".to_string(),
                    name: "マネージャー".to_string(),
                    role: "manager".to_string(),
                    status: "online".to_string(),
                    state: "idle".to_string(),
                },
                EmployeeInfo {
                    id: "dev_f".to_string(),
                    name: "デブエフ".to_string(),
                    role: "worker".to_string(),
                    status: "offline".to_string(),
                    state: String::new(),
                },
            ],
            ledger: BTreeMap::from([("mgr".to_string(), 512i64)]),
            relationships: vec![protocol::RelationshipInfo {
                from_id: "mgr".to_string(),
                to_id: "dev_f".to_string(),
                affinity: 15,
                trust: 70,
            }],
            tasks: vec![
                task("t1", "レポート", "done", "dev_m"),
                task("t2", "調査", "working", "dev_f"),
            ],
            ts: "t".to_string(),
        });
        app.apply(&ServerMsg::Notice {
            channel: "#会議室".to_string(),
            from: "mgr".to_string(),
            text: "今日のタスクは…".to_string(),
            ts: "t".to_string(),
        });
        app.status = ConnectionStatus::Connected;
        app.input = "/task レポート".to_string();

        let view = render_to_string(&app, 100, 30);
        // 全角文字は TestBackend 上で幅2のセル + 埋め草になるため、空白を除いて照合する。
        let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("社員"), "{view}");
        assert!(compact.contains("タスク"), "{view}");
        assert!(compact.contains("✔doneレポート(dev_m)"), "{view}");
        assert!(compact.contains("▶working調査(dev_f)"), "{view}");
        assert!(compact.contains("#会議室"), "{view}");
        assert!(compact.contains("マネージャー在席"), "{view}");
        assert!(compact.contains("デブエフ退勤"), "{view}");
        assert!(compact.contains("dev_f+15"), "{view}");
        assert!(compact.contains("学:512"), "{view}");
        assert!(compact.contains("mgr:今日のタスクは…"), "{view}");
        assert!(compact.contains('●'), "{view}");
        assert!(compact.contains('○'), "{view}");
        // 下部の入力行とヒント。
        assert!(compact.contains(">/taskレポート"), "{view}");
        assert!(compact.contains("[Enter]送信"), "{view}");
        assert!(compact.contains("[PgUp/PgDn]スクロール"), "{view}");
    }

    #[test]
    fn draw_input_line_shows_placeholder_and_hint() {
        // 入力が空のときは `_` プレースホルダを出す。
        let app = App::default();
        let view = render_to_string(&app, 100, 30);
        let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains(">_"), "{view}");
        assert!(compact.contains("[Enter]送信"), "{view}");
    }

    #[test]
    fn draw_shows_empty_tasks_placeholder() {
        // タスクが無いときはタスクペインに (なし) を出す。
        let app = App::default();
        let view = render_to_string(&app, 100, 30);
        let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("タスク"), "{view}");
        assert!(compact.contains("(なし)"), "{view}");
    }

    #[test]
    fn draw_does_not_panic_on_tiny_terminal() {
        // 極端に狭い端末でも saturating_sub で安全に描画できる。
        for (w, h) in [(1u16, 1u16), (5, 3), (10, 4), (20, 5)] {
            let _ = render_to_string(&App::default(), w, h);
        }
    }

    #[test]
    fn scroll_clamps_and_handles_empty() {
        let mut app = App::default();
        // 0 件でも何をしても安全。
        app.scroll_up(10);
        assert_eq!(app.scroll, 0);
        app.scroll_to_oldest();
        assert_eq!(app.scroll, 0);

        for i in 0..5 {
            app.push_local(format!("m{i}"));
        }
        assert_eq!(app.scroll, 0);

        app.scroll_up(2);
        assert_eq!(app.scroll, 2);
        app.scroll_down(1);
        assert_eq!(app.scroll, 1);
        app.scroll_down(100);
        assert_eq!(app.scroll, 0);

        // 上限は「件数 - 1」（最古）。
        app.scroll_up(100);
        assert_eq!(app.scroll, 4);
        app.scroll_to_newest();
        assert_eq!(app.scroll, 0);
        app.scroll_to_oldest();
        assert_eq!(app.scroll, 4);
    }

    #[test]
    fn new_messages_follow_at_bottom_and_keep_position_when_scrolled() {
        let mut app = App::default();
        for i in 0..5 {
            app.push_local(format!("m{i}"));
        }
        // 最新追従中（offset 0）は新着が来ても追従する。
        app.push_local("new".to_string());
        assert_eq!(app.scroll, 0);

        // スクロール中は相対位置を保つ。
        app.scroll_up(2);
        app.push_local("newer".to_string());
        assert_eq!(app.scroll, 2);
    }

    #[test]
    fn scroll_keys_do_not_modify_input_buffer() {
        let mut buf = String::from("abc");
        for code in [
            KeyCode::PageUp,
            KeyCode::PageDown,
            KeyCode::Home,
            KeyCode::End,
        ] {
            apply_input_key(&mut buf, code, KeyModifiers::NONE);
        }
        assert_eq!(buf, "abc");
    }

    #[test]
    fn addressed_say_is_sent_as_is() {
        // `@id` はサーバーが解釈するので、そのまま say として送る。
        assert_eq!(
            parse_input("@mgr 点呼"),
            InputAction::Say("@mgr 点呼".to_string())
        );
        assert_eq!(
            parse_input("@all 集合"),
            InputAction::Say("@all 集合".to_string())
        );
    }

    #[test]
    fn help_mentions_addresses_and_scroll_keys() {
        let help = HELP_LINES.join("\n");
        assert!(help.contains("@mgr"), "{help}");
        assert!(help.contains("@all"), "{help}");
        for key in ["PageUp", "PageDown", "Home", "End", "/task", "/quit"] {
            assert!(help.contains(key), "missing {key}: {help}");
        }
    }

    #[test]
    fn draw_keeps_messages_pane_on_small_terminals() {
        for (w, h) in [(20u16, 8u16), (30, 10), (40, 12)] {
            let mut app = App::default();
            app.push_local("こんにちは".to_string());
            let view = render_to_string(&app, w, h);
            let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
            assert!(compact.contains("#会議室"), "{w}x{h}:\n{view}");
        }
    }

    #[test]
    fn draw_hides_right_pane_on_narrow_terminals() {
        let app = App {
            online: vec!["mgr".to_string()],
            ..App::default()
        };
        // 狭い（2 ペイン未満）ときは mgr ペインを出さない。
        let narrow = render_to_string(&app, 20, 8);
        let compact: String = narrow.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("#会議室"), "{narrow}");
        assert!(!compact.contains("学:"), "{narrow}");

        // 広いときは mgr ペイン（学/状態/関係）を出す。
        let wide = render_to_string(&app, 100, 30);
        let compact: String = wide.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("学:-"), "{wide}");
        assert!(compact.contains("状態:-"), "{wide}");
        assert!(compact.contains("関係:"), "{wide}");
    }

    #[test]
    fn draw_shows_scroll_indicator_when_scrolled() {
        let mut app = App::default();
        for i in 0..30 {
            app.push_local(format!("m{i}"));
        }
        // 最新追従中はインジケータを出さない。
        let bottom = render_to_string(&app, 100, 30);
        let compact: String = bottom.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(!compact.contains("上に"), "{bottom}");

        app.scroll_up(3);
        let scrolled = render_to_string(&app, 100, 30);
        let compact: String = scrolled.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("上に3件"), "{scrolled}");
        assert!(compact.contains("Endで最新"), "{scrolled}");
    }

    #[test]
    fn parse_input_say_skip_and_cancel() {
        // `/say <本文>` は保留中でも発言する強制 Say。前後の空白は落とす。
        assert_eq!(
            parse_input("/say おはよう"),
            InputAction::SayForced("おはよう".to_string())
        );
        assert_eq!(
            parse_input("  /say   補足です  "),
            InputAction::SayForced("補足です".to_string())
        );
        // 本文が無い `/say` は何もしない（文字列 "/say" を発言にしない）。
        assert_eq!(parse_input("/say"), InputAction::Empty);
        assert_eq!(parse_input("/say   "), InputAction::Empty);
        // `/skip`・`/cancel` は保留中の質問への回答をやめる。
        assert_eq!(parse_input("/skip"), InputAction::SkipQuestion);
        assert_eq!(parse_input("/cancel"), InputAction::SkipQuestion);
        // `/sayfoo` は未知語として発言。
        assert_eq!(
            parse_input("/sayfoo"),
            InputAction::Say("/sayfoo".to_string())
        );
        // 通常文は Say（pending の有無による分岐は呼び側の run_ui が担う）。
        assert_eq!(
            parse_input("おはよう"),
            InputAction::Say("おはよう".to_string())
        );
    }

    #[test]
    fn apply_question_adds_notice_and_sets_pending() {
        let mut app = App::default();
        app.apply(&ServerMsg::Question {
            id: "q-1".to_string(),
            from: "dev_m".to_string(),
            text: "確認したいこと".to_string(),
            task_id: "t-1".to_string(),
            ts: "2026-09-28T09:00:00Z".to_string(),
        });
        assert_eq!(app.notices.len(), 1, "質問は 1 行だけ追加される");
        assert_eq!(app.notices[0].channel, DEFAULT_CHANNEL);
        assert_eq!(app.notices[0].from, "dev_m（質問）");
        assert_eq!(app.notices[0].text, "確認したいこと");
        assert!(!app.notices[0].local);
        assert_eq!(app.pending_question.as_deref(), Some("q-1"));
    }

    #[test]
    fn question_pending_is_independent_of_welcome_notice_clear() {
        let mut app = App::default();
        app.apply(&ServerMsg::Question {
            id: "q-1".to_string(),
            from: "dev_m".to_string(),
            text: "確認したいこと".to_string(),
            task_id: String::new(),
            ts: "t".to_string(),
        });
        assert_eq!(app.notices.len(), 1);
        assert_eq!(app.pending_question.as_deref(), Some("q-1"));

        // welcome は notices をクリアするが、保留中の質問は独立して残す。
        app.apply(&welcome(&[]));
        assert!(app.notices.is_empty());
        assert_eq!(app.pending_question.as_deref(), Some("q-1"));
    }

    #[test]
    fn answer_json_and_question_parse_match_protocol() {
        // Client -> Server の answer 表現（サーバー担当と同じ名前）。
        let msg = ClientMsg::Answer {
            question_id: "q-1".to_string(),
            text: "その方針で進めてください".to_string(),
        };
        assert_eq!(
            msg.to_json(),
            r#"{"type":"answer","question_id":"q-1","text":"その方針で進めてください"}"#
        );

        // Server -> Client の question が Question にパースされる。
        let parsed = ServerMsg::parse(
            r#"{"type":"question","id":"q-1","from":"dev_m","text":"確認","task_id":"t-1","ts":"t"}"#,
        )
        .unwrap();
        assert_eq!(
            parsed,
            ServerMsg::Question {
                id: "q-1".to_string(),
                from: "dev_m".to_string(),
                text: "確認".to_string(),
                task_id: "t-1".to_string(),
                ts: "t".to_string(),
            }
        );
    }

    #[test]
    fn pending_mention_is_sent_as_say_and_keeps_pending() {
        let action = InputAction::Say("@mgr こんにちは".to_string());
        let (msg, pending) = resolve_outgoing(&action, Some("q-1"));
        assert_eq!(
            msg,
            Some(ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: "@mgr こんにちは".to_string(),
            })
        );
        assert_eq!(pending.as_deref(), Some("q-1"), "保留は残る");
    }

    #[test]
    fn pending_slash_command_is_sent_as_say_and_keeps_pending() {
        // `parse_input` では `/task x` は Task になるが、Say として渡された場合の振る舞いを確認する。
        let action = InputAction::Say("/task x".to_string());
        let (msg, pending) = resolve_outgoing(&action, Some("q-1"));
        assert_eq!(
            msg,
            Some(ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: "/task x".to_string(),
            })
        );
        assert_eq!(pending.as_deref(), Some("q-1"), "保留は残る");
    }

    #[test]
    fn pending_plain_text_becomes_answer_and_clears_pending() {
        let action = InputAction::Say("了解です".to_string());
        let (msg, pending) = resolve_outgoing(&action, Some("q-1"));
        assert_eq!(
            msg,
            Some(ClientMsg::Answer {
                question_id: "q-1".to_string(),
                text: "了解です".to_string(),
            })
        );
        assert_eq!(pending, None, "保留はクリアされる");
    }

    #[test]
    fn no_pending_plain_text_is_sent_as_say() {
        let action = InputAction::Say("おはよう".to_string());
        let (msg, pending) = resolve_outgoing(&action, None);
        assert_eq!(
            msg,
            Some(ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: "おはよう".to_string(),
            })
        );
        assert_eq!(pending, None);
    }

    #[test]
    fn say_forced_sends_say_even_with_pending() {
        let action = InputAction::SayForced("強制発言".to_string());
        let (msg, pending) = resolve_outgoing(&action, Some("q-1"));
        assert_eq!(
            msg,
            Some(ClientMsg::Say {
                channel: DEFAULT_CHANNEL.to_string(),
                text: "強制発言".to_string(),
            })
        );
        assert_eq!(pending.as_deref(), Some("q-1"), "保留は残る");
    }

    #[test]
    fn skip_question_clears_pending_without_sending() {
        let (msg, pending) = resolve_outgoing(&InputAction::SkipQuestion, Some("q-1"));
        assert_eq!(msg, None, "送信しない");
        assert_eq!(pending, None, "保留はクリアされる");
    }

    #[test]
    fn non_say_actions_leave_pending_untouched() {
        // 副作用系（Help/Task など）はここでは何も決めず、保留も変えない。
        for action in [
            InputAction::Empty,
            InputAction::Quit,
            InputAction::Help,
            InputAction::TaskUsage,
            InputAction::Task {
                title: "t".to_string(),
                workspace: String::new(),
            },
        ] {
            let (msg, pending) = resolve_outgoing(&action, Some("q-1"));
            assert_eq!(msg, None, "送信しない");
            assert_eq!(pending.as_deref(), Some("q-1"), "保留は変えない");
        }
    }

    #[test]
    fn draw_input_prompt_shows_answering_when_question_pending() {
        let app = App {
            pending_question: Some("q-1".to_string()),
            ..App::default()
        };
        let view = render_to_string(&app, 100, 30);
        let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains("回答>"), "{view}");

        // 保留が無ければ従来どおり `> ` プロンプト。
        let view = render_to_string(&App::default(), 100, 30);
        let compact: String = view.chars().filter(|c| !c.is_whitespace()).collect();
        assert!(compact.contains(">_"), "{view}");
        assert!(!compact.contains("回答>"), "{view}");
    }
}
