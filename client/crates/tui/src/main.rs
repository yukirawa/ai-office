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

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::Result;
use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};
use futures_util::stream::SplitSink;
use futures_util::{SinkExt, StreamExt};
use protocol::{ClientMsg, EmployeeInfo, RelationshipInfo, ServerMsg, TaskInfo};
use ratatui::layout::{Constraint, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, List, ListItem, Paragraph, Wrap};
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
/// フレーム更新間隔（約10fps）。
const FRAME_INTERVAL: Duration = Duration::from_millis(100);
/// heartbeat の送信間隔（§4.1 の推奨 30 秒）。
const HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
/// 発言の既定チャンネル（§15.1）。
const DEFAULT_CHANNEL: &str = "#会議室";
/// 入力行に出すヒント。
const INPUT_HINT: &str = "[Enter] 送信  [/task タイトル] 依頼  [/help]  [/quit or Esc] 終了";
/// 入力行の右端に出す接続状態の最大表示幅。
const MAX_STATUS_WIDTH: u16 = 28;
/// `/task` にタイトルが無いときの使い方。
const TASK_USAGE: &str = "使い方: /task <タイトル>";
/// `/help` の表示内容。
const HELP_LINES: &[&str] = &[
    "コマンド:",
    "  /task <タイトル>  タスクを依頼",
    "  /help             このヘルプを表示",
    "  /quit, /exit      終了",
    "  その他の入力      #会議室 へ発言",
    "キー: Enter=送信 / Ctrl-U=クリア / Esc・Ctrl-C=終了 (q は入力文字)",
];

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;
type WsSink = SplitSink<WsStream, Message>;

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
    /// TUI ローカルの表示（送信エコー / ヘルプ / システム行）。サーバー由来ではない。
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
    /// 下部の入力バッファ（カーソルは常に末尾）。
    input: String,
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
            input: String::new(),
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
            ServerMsg::TaskAssign { .. }
            | ServerMsg::TaskCancel { .. }
            | ServerMsg::Error { .. }
            | ServerMsg::Unknown => {}
        }
    }

    /// TUI ローカルの行（送信エコー / ヘルプ / システム行）を追加する。
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
        }
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

    let cfg = Config::from_env();
    if snapshot {
        run_snapshot(cfg)
    } else {
        run_tui(cfg)
    }
}

// ---------------------------------------------------------------------------
// ヘッドレススナップショットモード
// ---------------------------------------------------------------------------

fn run_snapshot(cfg: Config) -> Result<()> {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()?;

    let mut app = App::default();
    rt.block_on(async {
        let collect = connect_and_collect(&cfg, &mut app, Duration::from_secs(SNAPSHOT_SECS));
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

/// 接続して `hello` を送り、`duration` のあいだ受信して `app` を更新する。
async fn connect_and_collect(cfg: &Config, app: &mut App, duration: Duration) -> Result<()> {
    let (ws, _resp) = tokio_tungstenite::connect_async(cfg.url.as_str()).await?;
    let (mut sink, mut stream) = ws.split();

    let hello = ClientMsg::Hello {
        employee_id: cfg.tui_id.clone(),
        device_id: "tui".to_string(),
        version: cfg.version.clone(),
    };
    sink.send(Message::text(hello.to_json())).await?;
    app.status = ConnectionStatus::Connected;

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
        {
            let state = app.lock().expect("app mutex poisoned");
            terminal.draw(|frame| draw(frame, &state))?;
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
                    match parse_input(&input) {
                        InputAction::Empty => {}
                        InputAction::Quit => quit = true,
                        InputAction::Help => {
                            for line in HELP_LINES {
                                state.push_local((*line).to_string());
                            }
                        }
                        InputAction::TaskUsage => state.push_local(TASK_USAGE.to_string()),
                        InputAction::Task(title) => {
                            // サーバーのエコーを待たずに即時フィードバックを出す。
                            state.push_local(format!("> /task {title}"));
                            outgoing = Some(ClientMsg::Task {
                                title,
                                description: String::new(),
                                mode: "local".to_string(),
                                repo: String::new(),
                                base_branch: String::new(),
                            });
                        }
                        InputAction::Say(text) => {
                            state.push_local(format!("> {text}"));
                            outgoing = Some(ClientMsg::Say {
                                channel: DEFAULT_CHANNEL.to_string(),
                                text,
                            });
                        }
                    }
                }
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
    let panes = Layout::horizontal([
        Constraint::Percentage(28),
        Constraint::Percentage(44),
        Constraint::Percentage(28),
    ])
    .split(chunks[0]);

    // 左列だけを上下に分割する（社員 60% / タスク 40%）。全3列の幅は変えない。
    let left =
        Layout::vertical([Constraint::Percentage(60), Constraint::Percentage(40)]).split(panes[0]);

    draw_employees(frame, left[0], app);
    draw_tasks(frame, left[1], app);
    draw_notices(frame, panes[1], app);
    draw_mgr(frame, panes[2], app);
    draw_input(frame, chunks[1], app);
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
    let block = Block::bordered().title(Line::styled(
        DEFAULT_CHANNEL,
        Style::default().add_modifier(Modifier::BOLD),
    ));
    let inner = block.inner(area);

    let lines: Vec<Line> = app
        .notices
        .iter()
        .map(|n| {
            if n.local {
                // 送信エコー / ヘルプ / システム行は一目で区別できるよう色を変える。
                Line::from(Span::styled(
                    n.text.clone(),
                    Style::default().fg(Color::Cyan),
                ))
            } else {
                // 送信者は太字、本文は通常。
                Line::from(vec![
                    Span::styled(
                        format!("{}: ", n.from),
                        Style::default().add_modifier(Modifier::BOLD),
                    ),
                    Span::raw(n.text.clone()),
                ])
            }
        })
        .collect();
    let total = lines.len();
    let offset = total.saturating_sub(inner.height as usize) as u16;

    let paragraph = Paragraph::new(lines)
        .block(block)
        .wrap(Wrap { trim: false })
        .scroll((offset, 0));
    frame.render_widget(paragraph, area);
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
    // プロンプト領域を最低1桁残してからヒント幅を決める（狭い端末でも安全）。
    let hint_width = (UnicodeWidthStr::width(INPUT_HINT) as u16)
        .min(area.width.saturating_sub(status_width).saturating_sub(1));

    let cols = Layout::horizontal([
        Constraint::Min(1),
        Constraint::Length(hint_width),
        Constraint::Length(status_width),
    ])
    .split(area);

    // 入力行（カーソルは常に末尾なので、空でなければカーソル記号を添える）。
    let mut spans = vec![Span::styled(
        "> ",
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
    /// タスク投入（タイトル）。
    Task(String),
    /// タイトル無しの `/task`。
    TaskUsage,
    /// #会議室 への発言。
    Say(String),
}

/// 入力行の文字列を解釈する（§15.2）。
///
/// - `/task <タイトル>` → [`InputAction::Task`]（タイトル必須）
/// - `/help` / `/quit` / `/exit`
/// - それ以外の非空入力 → [`InputAction::Say`]
/// - 空白のみ → [`InputAction::Empty`]
fn parse_input(input: &str) -> InputAction {
    let input = input.trim();
    if input.is_empty() {
        return InputAction::Empty;
    }
    match input {
        "/quit" | "/exit" => return InputAction::Quit,
        "/help" => return InputAction::Help,
        "/task" => return InputAction::TaskUsage,
        _ => {}
    }
    if let Some(rest) = input.strip_prefix("/task") {
        let mut chars = rest.chars();
        match chars.next() {
            // `/task` 単体は上で処理済みだが、念のため。
            None => return InputAction::TaskUsage,
            // `/task <タイトル>`（空白区切り）だけをコマンドとして扱う。
            Some(c) if c.is_whitespace() => {
                let title = chars.as_str().trim();
                if title.is_empty() {
                    return InputAction::TaskUsage;
                }
                return InputAction::Task(title.to_string());
            }
            // `/taskfoo` のような未知の語は発言として扱う。
            Some(_) => {}
        }
    }
    InputAction::Say(input.to_string())
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
        App, ConnectionStatus, InputAction, apply_input_key, backoff_delay, parse_input,
        status_display, task_status_prefix, task_summary_lines, truncate_to_width,
    };
    use crossterm::event::{KeyCode, KeyModifiers};
    use protocol::{EmployeeInfo, ServerMsg, TaskInfo};
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
        assert_eq!(parse_input("/task x"), InputAction::Task("x".to_string()));
        assert_eq!(
            parse_input("/task   レポート作成"),
            InputAction::Task("レポート作成".to_string())
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
        assert!(compact.contains("[/quitorEsc]終了"), "{view}");
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
}
