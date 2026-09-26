//! ai-office worker（Phase 0.3）。
//!
//! サーバーへアウトバウンド WebSocket 接続し、`hello` → `welcome`、定期 `heartbeat`、
//! 終了時 `bye` を送る常駐プロセス。Phase 0 では受信メッセージはログ出力のみ。
//!
//! 環境変数:
//! - `OFFICE_SERVER_URL`    既定 `ws://127.0.0.1:8787/ws`
//! - `OFFICE_EMPLOYEE_ID`   既定: 第1 CLI 引数（未設定ならエラー終了）
//! - `OFFICE_DEVICE_ID`     既定: ホスト名
//! - `OFFICE_HEARTBEAT_SECS` 既定 30
//! - `OFFICE_MAX_RETRIES`   既定 0（0 は無限リトライ）
//! - `OFFICE_VERSION`       既定 `0.1.0`

use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::stream::SplitSink;
use futures_util::{SinkExt, StreamExt};
use protocol::{ClientMsg, ServerMsg};
use tokio::net::TcpStream;
use tokio::sync::watch;
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

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;
type WsSink = SplitSink<WsStream, Message>;

/// 実行時設定。
#[derive(Debug, Clone)]
struct Config {
    url: String,
    employee_id: String,
    device_id: String,
    heartbeat_secs: u64,
    max_retries: u64,
    version: String,
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

        Ok(Self {
            url: env_string("OFFICE_SERVER_URL", DEFAULT_URL),
            employee_id,
            device_id: env_string("OFFICE_DEVICE_ID", &hostname()),
            heartbeat_secs: env_u64("OFFICE_HEARTBEAT_SECS", DEFAULT_HEARTBEAT_SECS).max(1),
            max_retries: env_u64("OFFICE_MAX_RETRIES", 0),
            version: env_string("OFFICE_VERSION", DEFAULT_VERSION),
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

    // heartbeat 用タイマー。interval の初回 tick は即時完了するので消費しておく。
    let mut heartbeat = interval(Duration::from_secs(cfg.heartbeat_secs));
    heartbeat.tick().await;

    loop {
        tokio::select! {
            res = shutdown.changed() => {
                // 送信側が消えた場合も終了扱いにする。
                if res.is_err() || *shutdown.borrow() {
                    let _ = send_bye(&mut sink).await;
                    let _ = sink.close().await;
                    log("closing connection");
                    return Ok(SessionEnd::Shutdown);
                }
            }
            _ = heartbeat.tick() => {
                let hb = ClientMsg::Heartbeat { ts: now_rfc3339() };
                if let Err(err) = sink.send(Message::text(hb.to_json())).await {
                    return Ok(SessionEnd::Disconnected {
                        reason: format!("heartbeat send failed: {err}"),
                    });
                }
                log("sent heartbeat");
            }
            incoming = stream.next() => {
                match incoming {
                    Some(Ok(msg)) => {
                        if let Some(text) = text_of(&msg) {
                            match ServerMsg::parse(text) {
                                Ok(m) => log_server_msg(&m),
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

/// `bye` を送る。
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
        ServerMsg::Error { code, message } => {
            log(format!("server error: code={code} message={message}"));
        }
        ServerMsg::OfficeState {
            online,
            employees,
            ledger,
            relationships,
            ts,
        } => {
            log(format!(
                "office_state: online=[{}] employees={} ledger={} relationships={} ts={ts}",
                online.join(", "),
                employees.len(),
                ledger.len(),
                relationships.len()
            ));
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
    use super::backoff_delay;
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
}
