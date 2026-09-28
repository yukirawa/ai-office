//! ai-office worker（Phase 0.3 + Phase 2/3）。
//!
//! サーバーへアウトバウンド WebSocket 接続し、`hello` → `welcome`、定期 `heartbeat`、
//! 終了時 `bye` を送る常駐プロセス。`task_assign` を受信したら Local / Remote モードで
//! 実行し、`task_result`（必要なら `task_progress`）を返す。
//!
//! 環境変数:
//! - `OFFICE_SERVER_URL`    既定 `ws://127.0.0.1:8787/ws`
//! - `OFFICE_EMPLOYEE_ID`   既定: 第1 CLI 引数（未設定ならエラー終了）
//! - `OFFICE_DEVICE_ID`     既定: ホスト名
//! - `OFFICE_HEARTBEAT_SECS` 既定 30
//! - `OFFICE_MAX_RETRIES`   既定 0（0 は無限リトライ）
//! - `OFFICE_VERSION`       既定 `0.1.0`
//! - `OFFICE_WORKSPACE`     local モードの作業ディレクトリ（local では必須）
//! - `OFFICE_ALLOWED_ROOTS` タスクで作業先に指定できる許可ルート（`:` 区切り）。
//!                          `OFFICE_WORKSPACE` 自体も常に含まれる
//! - `OFFICE_SANDBOX`       `bwrap`（既定） | `none`
//! - `OFFICE_ALLOW_EXEC`    `1` のときだけ `exec` を許可
//! - `OFFICE_GITHUB_API_URL` 既定 `https://api.github.com`

mod local;
mod remote;
mod sandbox;

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::stream::SplitSink;
use futures_util::{SinkExt, StreamExt};
use protocol::{Artifact, ClientMsg, ServerMsg, TaskAssignPayload};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, watch};
use tokio::time::{interval, timeout};
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

const DEFAULT_URL: &str = "ws://127.0.0.1:8787/ws";
const DEFAULT_VERSION: &str = "0.1.0";
const DEFAULT_HEARTBEAT_SECS: u64 = 30;
const BACKOFF_BASE_SECS: u64 = 1;
const BACKOFF_MAX_SECS: u64 = 30;
/// `welcome` を待つ上限。サーバーが沈黙したままハングしないための保険。
const WELCOME_TIMEOUT_SECS: u64 = 15;
/// 送信チャネルの容量（heartbeat / task_result / task_progress をここに積む）。
const OUTBOX_CAPACITY: usize = 64;
/// 終了時にライタータスクを待つ上限。
const WRITER_DRAIN_SECS: u64 = 5;

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;
type WsSink = SplitSink<WsStream, Message>;
/// 送信メッセージ（JSON テキスト）。ライタータスクが WebSocket へ流す。
type Outbox = mpsc::Sender<String>;
/// タスクの協調キャンセルフラグ。
type CancelFlag = Arc<AtomicBool>;
/// 実行中タスクの task_id -> キャンセルフラグ。
type Registry = Arc<Mutex<HashMap<String, CancelFlag>>>;

/// 実行時設定。
#[derive(Debug, Clone)]
struct Config {
    url: String,
    employee_id: String,
    device_id: String,
    heartbeat_secs: u64,
    max_retries: u64,
    version: String,
    /// local モードの作業ディレクトリ。
    workspace: Option<PathBuf>,
    /// タスクで作業先に指定できる許可ルート（`OFFICE_ALLOWED_ROOTS` + workspace）。
    allowed_roots: Vec<PathBuf>,
    /// `OFFICE_ALLOW_EXEC=1` のときだけ true。
    allow_exec: bool,
    /// `OFFICE_SANDBOX` の生値（解釈は local 実行時）。
    sandbox_raw: String,
    /// GitHub API のベース URL。
    github_api_url: String,
}

impl Config {
    fn from_env() -> Result<Self> {
        let employee_id = std::env::var("OFFICE_EMPLOYEE_ID")
            .ok()
            .filter(|s| !s.is_empty())
            .or_else(|| std::env::args().nth(1))
            .unwrap_or_default();
        if employee_id.is_empty() {
            anyhow::bail!(
                "OFFICE_EMPLOYEE_ID is required \
                 (set the env var or pass the employee id as the first CLI argument)"
            );
        }

        let workspace = env_opt_path("OFFICE_WORKSPACE");
        // `OFFICE_ALLOWED_ROOTS` を読み、`OFFICE_WORKSPACE` 自体も常に許可する。
        let allowed_roots =
            merge_allowed_roots(workspace.as_ref(), env_path_list("OFFICE_ALLOWED_ROOTS"));

        Ok(Self {
            url: env_string("OFFICE_SERVER_URL", DEFAULT_URL),
            employee_id,
            device_id: env_string("OFFICE_DEVICE_ID", &hostname()),
            heartbeat_secs: env_u64("OFFICE_HEARTBEAT_SECS", DEFAULT_HEARTBEAT_SECS).max(1),
            max_retries: env_u64("OFFICE_MAX_RETRIES", 0),
            version: env_string("OFFICE_VERSION", DEFAULT_VERSION),
            workspace,
            allowed_roots,
            allow_exec: env_flag("OFFICE_ALLOW_EXEC"),
            sandbox_raw: std::env::var("OFFICE_SANDBOX").unwrap_or_default(),
            github_api_url: env_string("OFFICE_GITHUB_API_URL", remote::DEFAULT_API_BASE),
        })
    }
}

/// セッションがどう終わったか。
enum SessionEnd {
    /// シグナルを受け、`bye` を送って正常終了した。
    Shutdown,
    /// 接続が切れた（理由つき）。再接続判断は呼び出し側。
    Disconnected { reason: String },
}

#[tokio::main]
async fn main() -> Result<()> {
    let cfg = match Config::from_env() {
        Ok(cfg) => cfg,
        Err(err) => {
            eprintln!("error: {err}");
            std::process::exit(2);
        }
    };

    log(format!(
        "worker starting: employee_id={} device_id={} version={} url={} heartbeat={}s max_retries={}",
        cfg.employee_id, cfg.device_id, cfg.version, cfg.url, cfg.heartbeat_secs, cfg.max_retries
    ));
    log(format!(
        "task config: workspace={} allow_exec={} sandbox={} github_api={}",
        cfg.workspace
            .as_ref()
            .map(|p| p.display().to_string())
            .unwrap_or_else(|| "(unset)".to_string()),
        cfg.allow_exec,
        if cfg.sandbox_raw.trim().is_empty() {
            "bwrap"
        } else {
            cfg.sandbox_raw.trim()
        },
        cfg.github_api_url,
    ));
    log(format!(
        "task allowed_roots: {}",
        if cfg.allowed_roots.is_empty() {
            "(none)".to_string()
        } else {
            cfg.allowed_roots
                .iter()
                .map(|p| p.display().to_string())
                .collect::<Vec<_>>()
                .join(":")
        }
    ));

    // SIGINT / SIGTERM を watch チャネルに流す。
    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    tokio::spawn(async move {
        wait_for_signal().await;
        log("signal received, shutting down");
        let _ = shutdown_tx.send(true);
    });

    run(cfg, shutdown_rx).await
}

/// 接続 → 切断を繰り返すメインループ。指数バックオフで再接続する。
async fn run(cfg: Config, mut shutdown: watch::Receiver<bool>) -> Result<()> {
    let mut retries: u64 = 0;

    loop {
        if *shutdown.borrow() {
            return Ok(());
        }

        match connect_and_run(&cfg, &mut shutdown).await {
            Ok(SessionEnd::Shutdown) => {
                log("shutdown complete");
                return Ok(());
            }
            Ok(SessionEnd::Disconnected { reason }) => {
                log(format!("disconnected: {reason}"));
            }
            Err(err) => {
                log(format!("connection error: {err:#}"));
            }
        }

        if *shutdown.borrow() {
            return Ok(());
        }

        // OFFICE_MAX_RETRIES が 0 のときは無限にリトライする。
        if cfg.max_retries != 0 && retries >= cfg.max_retries {
            anyhow::bail!("giving up after {retries} retries");
        }
        retries += 1;

        let delay = backoff_delay(retries);
        log(format!(
            "reconnecting in {}s (attempt {}{})",
            delay.as_secs(),
            retries,
            if cfg.max_retries == 0 {
                String::new()
            } else {
                format!("/{}", cfg.max_retries)
            }
        ));

        tokio::select! {
            _ = tokio::time::sleep(delay) => {}
            _ = shutdown.changed() => {
                log("shutdown during backoff");
                return Ok(());
            }
        }
    }
}

/// 1 セッション分。接続、hello、welcome 待ち、heartbeat、受信ループ。
async fn connect_and_run(cfg: &Config, shutdown: &mut watch::Receiver<bool>) -> Result<SessionEnd> {
    log(format!("connecting to {}", cfg.url));
    let (ws, _resp) = tokio_tungstenite::connect_async(cfg.url.as_str())
        .await
        .with_context(|| format!("failed to connect to {}", cfg.url))?;
    log("connected");

    let (mut sink, mut stream) = ws.split();

    let hello = ClientMsg::Hello {
        employee_id: cfg.employee_id.clone(),
        device_id: cfg.device_id.clone(),
        version: cfg.version.clone(),
    };
    sink.send(Message::text(hello.to_json()))
        .await
        .context("failed to send hello")?;
    log(format!("sent hello (employee_id={})", cfg.employee_id));

    // welcome を受信するまで待つ。
    let deadline = Duration::from_secs(WELCOME_TIMEOUT_SECS);
    loop {
        if *shutdown.borrow() {
            let _ = send_bye(&mut sink).await;
            return Ok(SessionEnd::Shutdown);
        }
        match timeout(deadline, stream.next()).await {
            Err(_) => {
                return Ok(SessionEnd::Disconnected {
                    reason: "timed out waiting for welcome".to_string(),
                });
            }
            Ok(None) => {
                return Ok(SessionEnd::Disconnected {
                    reason: "connection closed before welcome".to_string(),
                });
            }
            Ok(Some(Err(err))) => {
                return Ok(SessionEnd::Disconnected {
                    reason: format!("read error: {err}"),
                });
            }
            Ok(Some(Ok(msg))) => {
                let Some(text) = text_of(&msg) else { continue };
                match ServerMsg::parse(text) {
                    Ok(ServerMsg::Welcome {
                        session_id,
                        server_time,
                        office,
                    }) => {
                        log(format!(
                            "welcome: session_id={session_id} server_time={server_time} online=[{}]",
                            office.online.join(", ")
                        ));
                        break;
                    }
                    Ok(ServerMsg::Error { code, message }) => {
                        return Ok(SessionEnd::Disconnected {
                            reason: format!("server error {code}: {message}"),
                        });
                    }
                    Ok(other) => log_server_msg(&other),
                    Err(err) => log(format!("failed to parse server message: {err}")),
                }
            }
        }
    }

    // 送信はライタータスクへ集約する。heartbeat / task_result / task_progress は
    // すべて outbox 経由になり、読み取りループを塞がない。
    let (out_tx, mut out_rx) = mpsc::channel::<String>(OUTBOX_CAPACITY);
    let mut writer = tokio::spawn(async move {
        while let Some(text) = out_rx.recv().await {
            if sink.send(Message::text(text)).await.is_err() {
                break;
            }
        }
        let _ = sink.close().await;
    });

    let registry: Registry = Arc::new(Mutex::new(HashMap::new()));

    // heartbeat 用タイマー。interval の初回 tick は即時完了するので消費しておく。
    let mut heartbeat = interval(Duration::from_secs(cfg.heartbeat_secs));
    heartbeat.tick().await;

    loop {
        tokio::select! {
            res = shutdown.changed() => {
                // 送信側が消えた場合も終了扱いにする。
                if res.is_err() || *shutdown.borrow() {
                    let bye = ClientMsg::Bye { reason: "shutdown".to_string() };
                    let _ = out_tx.send(bye.to_json()).await;
                    drop(out_tx);
                    let _ = timeout(Duration::from_secs(WRITER_DRAIN_SECS), &mut writer).await;
                    log("closing connection");
                    return Ok(SessionEnd::Shutdown);
                }
            }
            _ = heartbeat.tick() => {
                let hb = ClientMsg::Heartbeat { ts: now_rfc3339() };
                if out_tx.send(hb.to_json()).await.is_err() {
                    return Ok(SessionEnd::Disconnected {
                        reason: "outbox closed".to_string(),
                    });
                }
                log("sent heartbeat");
            }
            res = &mut writer => {
                let reason = match res {
                    Ok(()) => "writer task ended".to_string(),
                    Err(err) => format!("writer task failed: {err}"),
                };
                return Ok(SessionEnd::Disconnected { reason });
            }
            incoming = stream.next() => {
                match incoming {
                    Some(Ok(msg)) => {
                        if let Some(text) = text_of(&msg) {
                            match ServerMsg::parse(text) {
                                Ok(m) => handle_server_msg(m, cfg, &out_tx, &registry),
                                Err(err) => log(format!("failed to parse server message: {err}")),
                            }
                        }
                    }
                    Some(Err(err)) => {
                        return Ok(SessionEnd::Disconnected {
                            reason: format!("read error: {err}"),
                        });
                    }
                    None => {
                        return Ok(SessionEnd::Disconnected {
                            reason: "connection closed".to_string(),
                        });
                    }
                }
            }
        }
    }
}

/// 受信メッセージをログし、実行が必要なものをタスクとして起動する。
fn handle_server_msg(msg: ServerMsg, cfg: &Config, out: &Outbox, registry: &Registry) {
    log_server_msg(&msg);
    match msg {
        ServerMsg::TaskAssign { task_id, payload } => {
            let cancelled: CancelFlag = Arc::new(AtomicBool::new(false));
            registry
                .lock()
                .unwrap()
                .insert(task_id.clone(), cancelled.clone());
            let cfg = cfg.clone();
            let out = out.clone();
            let registry = registry.clone();
            // 実行を別タスクに逃がし、読み取りループ / heartbeat を止めない。
            tokio::spawn(async move {
                run_task(task_id, payload, cfg, out, cancelled, registry).await;
            });
        }
        ServerMsg::TaskCancel { task_id, reason } => {
            match registry.lock().unwrap().get(&task_id).cloned() {
                Some(flag) => {
                    flag.store(true, Ordering::SeqCst);
                    log(format!(
                        "task_cancel: task_id={task_id} reason={reason} (cancellation requested)"
                    ));
                }
                None => log(format!(
                    "task_cancel: task_id={task_id} reason={reason} (no running task)"
                )),
            }
        }
        _ => {}
    }
}

/// 実行結果（local / remote 共通）。
struct TaskOutcome {
    status: &'static str,
    summary: String,
    detail: String,
    artifacts: Vec<Artifact>,
}

fn fail(reason: impl Into<String>) -> TaskOutcome {
    let reason = reason.into();
    TaskOutcome {
        status: "failed",
        summary: reason.clone(),
        detail: reason,
        artifacts: Vec::new(),
    }
}

/// 1 タスクを実行し、必ず `task_result` を 1 通送る。
async fn run_task(
    task_id: String,
    payload: serde_json::Value,
    cfg: Config,
    out: Outbox,
    cancelled: CancelFlag,
    registry: Registry,
) {
    let outcome = execute_task(&task_id, &payload, &cfg, &out, &cancelled).await;

    // 後始末（登録解除）。
    registry.lock().unwrap().remove(&task_id);

    let msg = task_result_msg(&task_id, &outcome);
    match out.send(msg.to_json()).await {
        Ok(()) => log(format!(
            "task {task_id}: task_result sent (status={})",
            outcome.status
        )),
        Err(_) => log(format!(
            "task {task_id}: connection closed; task_result could not be sent"
        )),
    }
}

/// `TaskOutcome` を `task_result` メッセージに変換する（テスト可能にするため分離）。
fn task_result_msg(task_id: &str, outcome: &TaskOutcome) -> ClientMsg {
    ClientMsg::TaskResult {
        task_id: task_id.to_string(),
        status: outcome.status.to_string(),
        summary: outcome.summary.clone(),
        detail: outcome.detail.clone(),
        artifacts: outcome.artifacts.clone(),
    }
}

/// payload を解釈してモード別に実行する。失敗は必ず `failed` の [`TaskOutcome`] にする。
async fn execute_task(
    task_id: &str,
    payload: &serde_json::Value,
    cfg: &Config,
    out: &Outbox,
    cancelled: &CancelFlag,
) -> TaskOutcome {
    let payload: TaskAssignPayload = match serde_json::from_value(payload.clone()) {
        Ok(p) => p,
        Err(err) => return fail(format!("task_assign の payload を解析できません: {err}")),
    };

    let mode = if payload.mode.trim().is_empty() {
        // 未指定は既定の local として扱う（§13.4 の tasks.mode 既定に合わせる）。
        "local"
    } else {
        payload.mode.trim()
    };

    match mode {
        "local" => {
            // タスクごとの作業先を決める（未指定なら `OFFICE_WORKSPACE`）。
            let root = match local::resolve_task_root(
                &payload.workspace,
                cfg.workspace.as_ref(),
                &cfg.allowed_roots,
            ) {
                Ok(root) => root,
                Err(reason) => return fail(reason),
            };
            log(format!("task {task_id}: workspace={}", root.display()));
            let env = build_local_env(cfg, Some(root));
            let progress_task_id = task_id.to_string();
            let progress_out = out.clone();
            let cancelled = cancelled.clone();
            let joined = tokio::task::spawn_blocking(move || {
                let mut on_progress = move |percent: u8, message: &str| {
                    let msg = ClientMsg::TaskProgress {
                        task_id: progress_task_id.clone(),
                        message: message.to_string(),
                        percent,
                    };
                    let _ = progress_out.try_send(msg.to_json());
                };
                local::execute_local(&payload, &env, &cancelled, &mut on_progress)
            })
            .await;

            match joined {
                Ok(r) => TaskOutcome {
                    status: r.status,
                    summary: r.summary,
                    detail: r.detail,
                    artifacts: r.artifacts,
                },
                Err(err) => fail(format!("local 実行タスクが失敗しました: {err}")),
            }
        }
        "remote" => {
            let Some(spec) = payload.remote.clone() else {
                return fail("remote モードですが remote 指定がありません".to_string());
            };
            let env = remote::RemoteEnv {
                api_base: cfg.github_api_url.clone(),
            };
            let progress_task_id = task_id.to_string();
            let progress_out = out.clone();
            let joined = tokio::task::spawn_blocking(move || {
                let mut on_progress = move |percent: u8, message: &str| {
                    let msg = ClientMsg::TaskProgress {
                        task_id: progress_task_id.clone(),
                        message: message.to_string(),
                        percent,
                    };
                    let _ = progress_out.try_send(msg.to_json());
                };
                remote::execute_remote(&spec, &env, &mut on_progress)
            })
            .await;

            match joined {
                Ok(r) => TaskOutcome {
                    status: r.status,
                    summary: r.summary,
                    detail: r.detail,
                    artifacts: r.artifacts,
                },
                Err(err) => fail(format!("remote 実行タスクが失敗しました: {err}")),
            }
        }
        other => fail(format!("未知の mode です: {other:?}")),
    }
}

/// `Config` から local 実行環境を組み立てる（bwrap 不在なら `none` に落とす）。
///
/// `root` はタスクごとに決めた作業ディレクトリ（未指定なら `OFFICE_WORKSPACE`）。
fn build_local_env(cfg: &Config, root: Option<PathBuf>) -> local::LocalEnv {
    let (mut mode, warning) = sandbox::SandboxMode::from_env_value(&cfg.sandbox_raw);
    if let Some(warning) = warning {
        log(warning);
    }
    if mode == sandbox::SandboxMode::Bwrap && !sandbox::bwrap_available() {
        log(
            "bwrap not found in PATH; falling back to OFFICE_SANDBOX=none \
             (workspace path checks are still enforced)",
        );
        mode = sandbox::SandboxMode::None;
    }
    local::LocalEnv {
        root,
        allow_exec: cfg.allow_exec,
        sandbox: mode,
    }
}

/// `OFFICE_ALLOWED_ROOTS` と `OFFICE_WORKSPACE` を統合する。
///
/// `OFFICE_WORKSPACE` 自体は常に許可ルートに含める（従来の挙動を保つため）。
fn merge_allowed_roots(workspace: Option<&PathBuf>, mut allowed: Vec<PathBuf>) -> Vec<PathBuf> {
    if let Some(ws) = workspace
        && !ws.as_os_str().is_empty()
        && !allowed.contains(ws)
    {
        allowed.push(ws.clone());
    }
    allowed
}

/// `bye` を送る（welcome 待ちの間のみ使用）。
async fn send_bye(sink: &mut WsSink) -> Result<()> {
    let bye = ClientMsg::Bye {
        reason: "shutdown".to_string(),
    };
    sink.send(Message::text(bye.to_json()))
        .await
        .context("failed to send bye")?;
    log("sent bye");
    Ok(())
}

/// 受信メッセージをログ出力する。
fn log_server_msg(msg: &ServerMsg) {
    match msg {
        ServerMsg::Welcome { session_id, .. } => {
            log(format!("welcome: session_id={session_id}"));
        }
        ServerMsg::Notice {
            channel,
            from,
            text,
            ts: _,
        } => {
            // 例: [#会議室] mgr: 今日のタスクは…
            log(format!("[{channel}] {from}: {text}"));
        }
        ServerMsg::TaskAssign { task_id, payload } => {
            log(format!("task_assign: task_id={task_id} payload={payload}"));
        }
        ServerMsg::TaskCancel { task_id, reason } => {
            log(format!("task_cancel: task_id={task_id} reason={reason}"));
        }
        ServerMsg::Error { code, message } => {
            log(format!("server error: code={code} message={message}"));
        }
        ServerMsg::OfficeState {
            online,
            employees,
            ledger,
            relationships,
            tasks,
            ts,
        } => {
            log(format!(
                "office_state: online=[{}] employees={} ledger={} relationships={} tasks={} ts={ts}",
                online.join(", "),
                employees.len(),
                ledger.len(),
                relationships.len(),
                tasks.len()
            ));
        }
        ServerMsg::Question {
            id,
            from,
            text,
            task_id: _,
            ts: _,
        } => {
            log(format!("question: id={id} from={from} text={text}"));
        }
        ServerMsg::Unknown => {
            log("unknown server message (ignored)");
        }
    }
}

/// Text フレームなら中身を返す。
fn text_of(msg: &Message) -> Option<&str> {
    match msg {
        Message::Text(text) => Some(text.as_str()),
        Message::Binary(_) | Message::Ping(_) | Message::Pong(_) => None,
        Message::Close(_) | Message::Frame(_) => None,
    }
}

/// SIGINT (Ctrl-C) または SIGTERM を待つ。
async fn wait_for_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut term = match signal(SignalKind::terminate()) {
            Ok(sig) => sig,
            Err(err) => {
                log(format!("failed to install SIGTERM handler: {err}"));
                let _ = tokio::signal::ctrl_c().await;
                return;
            }
        };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = term.recv() => {},
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
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

/// タイムスタンプ付きで stdout にログを出す。
fn log(msg: impl AsRef<str>) {
    println!("[{}] {}", now_rfc3339(), msg.as_ref());
}

/// 現在 UTC 時刻を RFC3339 風（秒精度）で返す。
fn now_rfc3339() -> String {
    chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ").to_string()
}

/// ホスト名を推定する（追加クレートなし）。
fn hostname() -> String {
    if let Ok(name) = std::env::var("HOSTNAME")
        && !name.trim().is_empty()
    {
        return name.trim().to_string();
    }
    if let Ok(name) = std::fs::read_to_string("/etc/hostname")
        && !name.trim().is_empty()
    {
        return name.trim().to_string();
    }
    if let Ok(output) = std::process::Command::new("hostname").output() {
        let name = String::from_utf8_lossy(&output.stdout);
        if !name.trim().is_empty() {
            return name.trim().to_string();
        }
    }
    "unknown".to_string()
}

/// 環境変数を読む。未設定または空文字なら既定値。
fn env_string(key: &str, default: &str) -> String {
    std::env::var(key)
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| default.to_string())
}

/// 空でない環境変数を `PathBuf` として読む。
fn env_opt_path(key: &str) -> Option<PathBuf> {
    std::env::var(key)
        .ok()
        .filter(|s| !s.is_empty())
        .map(PathBuf::from)
}

/// `:` 区切りのディレクトリ一覧を `PathBuf` の列として読む（空要素は無視）。
///
/// 未設定・空文字なら空ベクトル。
fn env_path_list(key: &str) -> Vec<PathBuf> {
    std::env::var(key)
        .ok()
        .map(|value| {
            value
                .split(':')
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .map(PathBuf::from)
                .collect()
        })
        .unwrap_or_default()
}

/// `1` / `true` / `yes`（大文字小文字を問わない）を真として読む。
fn env_flag(key: &str) -> bool {
    match std::env::var(key) {
        Ok(value) => matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes"
        ),
        Err(_) => false,
    }
}

/// u64 の環境変数を読む。未設定・空文字・パース失敗なら既定値。
fn env_u64(key: &str, default: u64) -> u64 {
    match std::env::var(key) {
        Ok(value) if !value.trim().is_empty() => match value.trim().parse() {
            Ok(parsed) => parsed,
            Err(err) => {
                log(format!(
                    "invalid {key}={value:?} ({err}); using default {default}"
                ));
                default
            }
        },
        _ => default,
    }
}

#[cfg(test)]
mod tests {
    use super::{backoff_delay, env_flag, merge_allowed_roots};
    use std::path::PathBuf;
    use std::time::Duration;

    #[test]
    fn backoff_is_exponential_with_cap() {
        assert_eq!(backoff_delay(1), Duration::from_secs(1));
        assert_eq!(backoff_delay(2), Duration::from_secs(2));
        assert_eq!(backoff_delay(3), Duration::from_secs(4));
        assert_eq!(backoff_delay(4), Duration::from_secs(8));
        assert_eq!(backoff_delay(5), Duration::from_secs(16));
        assert_eq!(backoff_delay(6), Duration::from_secs(30));
        assert_eq!(backoff_delay(100), Duration::from_secs(30));
    }

    #[test]
    fn env_flag_accepts_expected_values() {
        // 安全側: 未設定は false。
        unsafe { std::env::remove_var("OFFICE_TEST_FLAG") };
        assert!(!env_flag("OFFICE_TEST_FLAG"));
        unsafe { std::env::set_var("OFFICE_TEST_FLAG", "1") };
        assert!(env_flag("OFFICE_TEST_FLAG"));
        unsafe { std::env::set_var("OFFICE_TEST_FLAG", "true") };
        assert!(env_flag("OFFICE_TEST_FLAG"));
        unsafe { std::env::set_var("OFFICE_TEST_FLAG", "0") };
        assert!(!env_flag("OFFICE_TEST_FLAG"));
        unsafe { std::env::remove_var("OFFICE_TEST_FLAG") };
    }

    #[test]
    fn allowed_roots_always_include_workspace() {
        // OFFICE_WORKSPACE 自体は常に許可ルートに含められる。
        let ws = PathBuf::from("/home/user/Dev/office-ws");
        let roots = merge_allowed_roots(Some(&ws), vec![PathBuf::from("/home/user/Dev/site-a")]);
        assert!(roots.contains(&ws));
        assert!(roots.contains(&PathBuf::from("/home/user/Dev/site-a")));

        // 既に含まれていれば重複しない。
        let roots = merge_allowed_roots(Some(&ws), vec![ws.clone()]);
        assert_eq!(roots.len(), 1);

        // workspace 未設定ならそのまま。
        let roots = merge_allowed_roots(None, vec![PathBuf::from("/x")]);
        assert_eq!(roots, vec![PathBuf::from("/x")]);
    }

    #[test]
    fn task_result_message_carries_status_and_pr_url() {
        let outcome = super::TaskOutcome {
            status: "done",
            summary: "PR を作成しました: https://github.com/o/r/pull/7".to_string(),
            detail: "log".to_string(),
            artifacts: Vec::new(),
        };
        let msg = super::task_result_msg("t1", &outcome);
        let json = msg.to_json();
        assert!(json.contains(r#""type":"task_result""#));
        assert!(json.contains(r#""status":"done""#));
        assert!(json.contains("https://github.com/o/r/pull/7"));

        let failed = super::task_result_msg("t2", &super::fail("token 未設定"));
        let json = failed.to_json();
        assert!(json.contains(r#""status":"failed""#));
        assert!(json.contains("token 未設定"));
    }
}
