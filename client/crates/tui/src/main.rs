//! ai-office TUI（Phase 0.4）。
//!
//! read-only ビューア。`/ws` に接続し、サーバーから届く [`ServerMsg`] を画面に表示する。
//! 入力機能は Phase 1 以降。
//!
//! 環境変数:
//! - `OFFICE_SERVER_URL` 既定 `ws://127.0.0.1:8787/ws`
//! - `OFFICE_TUI_ID`   既定 `owner`
//!
//! ヘッドレススモークモード:
//! - `OFFICE_TUI_SNAPSHOT=1` または第1引数 `--snapshot` で、代替スクリーンに入らず
//!   約2秒収集してプレーンテキストのサマリを stdout に出し、exit 0 する。

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
use tokio::sync::watch;
use tokio::time::{Instant, sleep, timeout};
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
                });
                if self.notices.len() > MAX_NOTICES {
                    let excess = self.notices.len() - MAX_NOTICES;
                    self.notices.drain(0..excess);
                }
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
    let tasks: Vec<&TaskInfo> = app.tasks.iter().take(5).collect();
    println!("tasks (last {}):", tasks.len());
    if tasks.is_empty() {
        println!("  (なし)");
    } else {
        for t in tasks {
            println!(
                "  - status={} assignee={} mode={} title={}",
                t.status, t.assignee, t.mode, t.title
            );
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

    // WebSocket 受信タスク。ランタイムのワーカースレッド上で動く。
    {
        let app = Arc::clone(&app);
        rt.spawn(async move {
            ws_loop(cfg, app, shutdown_rx).await;
        });
    }

    // `ratatui::init()` は代替スクリーン + raw モード + panic 時の復元フックを設定する。
    let mut terminal = ratatui::init();
    let result = run_ui(&mut terminal, &app);
    ratatui::restore();

    // 受信タスクに bye を送る余地を与えつつ、確実に片付ける。
    let _ = shutdown_tx.send(true);
    rt.shutdown_timeout(Duration::from_millis(500));

    result
}

/// キー入力をポーリングしながら約10fpsで描画する。
fn run_ui(terminal: &mut DefaultTerminal, app: &Arc<Mutex<App>>) -> Result<()> {
    loop {
        {
            let state = app.lock().expect("app mutex poisoned");
            terminal.draw(|frame| draw(frame, &state))?;
        }

        if event::poll(FRAME_INTERVAL)?
            && let Event::Key(key) = event::read()?
            && key.kind == KeyEventKind::Press
        {
            match key.code {
                KeyCode::Char('q') | KeyCode::Char('Q') => return Ok(()),
                KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
                    return Ok(());
                }
                KeyCode::Esc => return Ok(()),
                _ => {}
            }
        }
    }
}

/// WebSocket 接続を維持し、切れたら指数バックオフで再接続する。
async fn ws_loop(cfg: Config, app: Arc<Mutex<App>>, mut shutdown: watch::Receiver<bool>) {
    let mut retries: u64 = 0;
    loop {
        if *shutdown.borrow() {
            return;
        }
        set_status(&app, ConnectionStatus::Connecting);

        match session(&cfg, &app, &mut shutdown).await {
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

async fn session(
    cfg: &Config,
    app: &Arc<Mutex<App>>,
    shutdown: &mut watch::Receiver<bool>,
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

    let list = List::new(items).block(Block::bordered().title("社員"));
    frame.render_widget(list, area);
}

fn draw_tasks(frame: &mut Frame, area: Rect, app: &App) {
    let block = Block::bordered().title("タスク");
    let inner = block.inner(area);
    // 枠線の内側の幅（プレフィックス分を差し引いておく）。
    let max_chars = (inner.width as usize).saturating_sub(1);

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
                let text = truncate_to_width(&text, max_chars);
                ListItem::new(Line::from(Span::styled(text, Style::default().fg(color))))
            })
            .collect()
    };

    frame.render_widget(List::new(items).block(block), area);
}

fn draw_notices(frame: &mut Frame, area: Rect, app: &App) {
    let block = Block::bordered().title("#会議室");
    let inner = block.inner(area);

    let lines: Vec<Line> = app
        .notices
        .iter()
        .map(|n| Line::from(format!("{}: {}", n.from, n.text)))
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

    let paragraph = Paragraph::new(lines).block(Block::bordered().title("mgr"));
    frame.render_widget(paragraph, area);
}

fn draw_input(frame: &mut Frame, area: Rect, app: &App) {
    let cols = Layout::horizontal([Constraint::Min(1), Constraint::Length(48)]).split(area);

    let prompt = Paragraph::new(Line::from(vec![
        Span::styled("> ", Style::default().add_modifier(Modifier::BOLD)),
        Span::styled("_", Style::default().fg(Color::DarkGray)),
    ]));
    frame.render_widget(prompt, cols[0]);

    let status = Paragraph::new(Line::from(Span::styled(
        app.status.label(),
        Style::default().fg(app.status.color()),
    )))
    .alignment(ratatui::layout::Alignment::Right);
    frame.render_widget(status, cols[1]);
}

// ---------------------------------------------------------------------------
// ヘルパ
// ---------------------------------------------------------------------------

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
        "review" => ("◀", Color::Cyan),
        "assigned" => ("○", Color::Blue),
        "pending" => ("·", Color::DarkGray),
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
        App, ConnectionStatus, backoff_delay, status_display, task_status_prefix, truncate_to_width,
    };
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

    #[test]
    fn draw_renders_panes_without_panicking() {
        use ratatui::Terminal;
        use ratatui::backend::TestBackend;

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

        let mut terminal = Terminal::new(TestBackend::new(100, 30)).unwrap();
        terminal.draw(|frame| super::draw(frame, &app)).unwrap();

        let buf = terminal.backend().buffer();
        let mut view = String::new();
        for y in 0..buf.area.height {
            for x in 0..buf.area.width {
                view.push_str(buf.cell((x, y)).map(|c| c.symbol()).unwrap_or(" "));
            }
            view.push('\n');
        }
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
    }
}
