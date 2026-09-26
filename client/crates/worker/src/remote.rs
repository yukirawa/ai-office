//! remote モードのタスク実行（設計書 §7.3, §13.5, Phase 3.2）。
//!
//! サーバーから渡された短命の installation token を使い、GitHub REST API を
//! 直接叩いてブランチ作成 → ファイル push → PR 作成まで行う。
//!
//! ネットワーク I/O は `ureq` によるブロッキング呼び出し。呼び出し側（main）が
//! `tokio::task::spawn_blocking` の中で実行する前提。

use std::time::Duration;

use base64::Engine as _;
use protocol::{Artifact, RemoteSpec};
use serde_json::{Value, json};

/// `OFFICE_GITHUB_API_URL` の既定値。
pub const DEFAULT_API_BASE: &str = "https://api.github.com";
const USER_AGENT: &str = "ai-office-worker";
const TIMEOUT_SECS: u64 = 30;
/// エラー本文を detail に載せるときの上限。
const MAX_ERROR_BODY: usize = 300;
/// 1 行の detail 上限。
const MAX_DETAIL_LINE: usize = 4096;

/// remote モードの実行環境。
#[derive(Debug, Clone)]
pub struct RemoteEnv {
    /// GitHub API のベース URL（末尾スラッシュなし）。
    pub api_base: String,
}

/// 実行結果。`status` は `"done"` | `"failed"`。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoteResult {
    pub status: &'static str,
    pub summary: String,
    pub detail: String,
    pub artifacts: Vec<Artifact>,
}

/// 共通ヘッダを付ける。
macro_rules! with_headers {
    ($rb:expr, $token:expr) => {
        $rb.header("Authorization", format!("Bearer {}", $token))
            .header("Accept", "application/vnd.github+json")
            .header("User-Agent", USER_AGENT)
    };
}

/// remote モードを実行する。
pub fn execute_remote(
    spec: &RemoteSpec,
    env: &RemoteEnv,
    on_progress: &mut dyn FnMut(u8, &str),
) -> RemoteResult {
    let mut detail: Vec<String> = Vec::new();
    let mut artifacts: Vec<Artifact> = Vec::new();
    match run_remote(spec, env, on_progress, &mut detail, &mut artifacts) {
        Ok(html_url) => RemoteResult {
            status: "done",
            summary: format!("PR を作成しました: {html_url}"),
            detail: detail.join("\n"),
            artifacts,
        },
        Err(reason) => RemoteResult {
            status: "failed",
            summary: reason,
            detail: detail.join("\n"),
            artifacts,
        },
    }
}

fn run_remote(
    spec: &RemoteSpec,
    env: &RemoteEnv,
    on_progress: &mut dyn FnMut(u8, &str),
    detail: &mut Vec<String>,
    artifacts: &mut Vec<Artifact>,
) -> Result<String, String> {
    let token = spec.token.trim();
    if token.is_empty() {
        return Err("token 未設定（remote モードには installation token が必要です）".to_string());
    }
    let repo = spec.repo.trim();
    let base_branch = spec.base_branch.trim();
    let branch = spec.branch.trim();
    if repo.is_empty() || base_branch.is_empty() || branch.is_empty() {
        return Err("remote の repo / base_branch / branch は必須です".to_string());
    }
    let api = env.api_base.trim_end_matches('/').to_string();
    let client = Client::new(token);

    // 1. base branch の SHA を取得。
    on_progress(5, "base branch の SHA を取得");
    let ref_url = base_ref_url(&api, repo, base_branch);
    let (status, body) = client.get(&ref_url)?;
    detail.push(format!("GET {ref_url} -> {status}"));
    if status != 200 {
        return Err(format!(
            "base branch の取得に失敗しました ({status}): {}",
            summarize(&body)
        ));
    }
    let base_sha = extract_sha(&body)
        .ok_or_else(|| "base branch の SHA を応答から取得できませんでした".to_string())?;

    // 2. 作業ブランチを作成（既存なら 422 を無視して継続）。
    on_progress(20, "ブランチを作成");
    let refs_url = format!("{api}/repos/{repo}/git/refs");
    let (status, body) = client.post_json(&refs_url, &ref_create_body(branch, &base_sha))?;
    detail.push(format!("POST {refs_url} -> {status}"));
    if status == 422 {
        detail.push("  (ブランチは既に存在するため継続)".to_string());
    } else if !is_success(status) {
        return Err(format!(
            "ブランチ作成に失敗しました ({status}): {}",
            summarize(&body)
        ));
    }

    // 3. 各ファイルを push。
    let total = spec.files.len();
    for (i, file) in spec.files.iter().enumerate() {
        let percent = if total == 0 {
            80
        } else {
            let step = (i * 60).checked_div(total).unwrap_or(0);
            20 + step as u8
        };
        on_progress(percent, &format!("push {}/{}: {}", i + 1, total, file.path));
        let contents_url = contents_url(&api, repo, file.path.trim());
        let content_b64 = base64::engine::general_purpose::STANDARD.encode(file.content.as_bytes());
        let message = if spec.title.trim().is_empty() {
            format!("ai-office: update {}", file.path)
        } else {
            spec.title.clone()
        };

        let (status, body) = client.put_json(
            &contents_url,
            &contents_put_body(&message, &content_b64, branch, None),
        )?;
        detail.push(format!("PUT {contents_url} -> {status}"));
        let mut final_status = status;

        if status == 422 {
            // 既存ファイル: 現在の sha を取得して再送する。
            let get_url = format!("{contents_url}?ref={}", encode_ref(branch));
            let (get_status, get_body) = client.get(&get_url)?;
            detail.push(format!("GET {get_url} -> {get_status}"));
            if !is_success(get_status) {
                return Err(format!(
                    "既存ファイルの sha 取得に失敗しました {}({get_status}): {}",
                    file.path,
                    summarize(&get_body)
                ));
            }
            let file_sha = extract_sha(&get_body).ok_or_else(|| {
                format!(
                    "既存ファイル {} の sha を応答から取得できませんでした",
                    file.path
                )
            })?;
            let body = contents_put_body(&message, &content_b64, branch, Some(&file_sha));
            let (retry_status, retry_body) = client.put_json(&contents_url, &body)?;
            detail.push(format!(
                "PUT {contents_url} (既存, sha 付き) -> {retry_status}"
            ));
            final_status = retry_status;
            if !is_success(final_status) {
                return Err(format!(
                    "ファイル更新に失敗しました {}({final_status}): {}",
                    file.path,
                    summarize(&retry_body)
                ));
            }
        } else if !is_success(final_status) {
            return Err(format!(
                "ファイル push に失敗しました {}({final_status}): {}",
                file.path,
                summarize(&body)
            ));
        }

        artifacts.push(Artifact {
            path: file.path.clone(),
            bytes: file.content.len() as u64,
        });
    }

    // 4. PR を作成。
    on_progress(90, "PR を作成");
    let pulls_url = format!("{api}/repos/{repo}/pulls");
    let (status, body) = client.post_json(
        &pulls_url,
        &pr_create_body(spec.title.trim(), branch, base_branch, spec.body.trim()),
    )?;
    detail.push(format!("POST {pulls_url} -> {status}"));
    if !is_success(status) {
        return Err(format!(
            "PR 作成に失敗しました ({status}): {}",
            summarize(&body)
        ));
    }
    let html_url = extract_str(&body, "html_url")
        .ok_or_else(|| "PR の html_url を応答から取得できませんでした".to_string())?;
    if let Some(number) = extract_u64(&body, "number") {
        detail.push(format!("  PR #{number}"));
    }
    on_progress(100, "完了");
    Ok(html_url)
}

/// GitHub REST への薄いクライアント。非 2xx も結果として返す（body を見たいため）。
struct Client {
    agent: ureq::Agent,
    token: String,
}

impl Client {
    fn new(token: &str) -> Self {
        let config = ureq::Agent::config_builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(TIMEOUT_SECS)))
            .build();
        Self {
            agent: config.into(),
            token: token.to_string(),
        }
    }

    fn get(&self, url: &str) -> Result<(u16, String), String> {
        let rb = with_headers!(self.agent.get(url), &self.token);
        finish(rb.call(), "GET", url)
    }

    fn post_json(&self, url: &str, body: &Value) -> Result<(u16, String), String> {
        let rb = with_headers!(self.agent.post(url), &self.token);
        finish(rb.send_json(body), "POST", url)
    }

    fn put_json(&self, url: &str, body: &Value) -> Result<(u16, String), String> {
        let rb = with_headers!(self.agent.put(url), &self.token);
        finish(rb.send_json(body), "PUT", url)
    }
}

fn finish(
    result: Result<ureq::http::Response<ureq::Body>, ureq::Error>,
    method: &str,
    url: &str,
) -> Result<(u16, String), String> {
    let resp = result.map_err(|err| format!("{method} {url} 失敗: {err}"))?;
    let status = resp.status().as_u16();
    let body = resp
        .into_body()
        .read_to_string()
        .map_err(|err| format!("{method} {url} 応答の読取に失敗: {err}"))?;
    Ok((status, body))
}

fn is_success(status: u16) -> bool {
    (200..300).contains(&status)
}

/// `GET .../git/ref/heads/<base>` の URL。
pub fn base_ref_url(api: &str, repo: &str, base_branch: &str) -> String {
    format!(
        "{api}/repos/{repo}/git/ref/heads/{}",
        encode_ref(base_branch)
    )
}

/// `.../contents/<path>` の URL。
pub fn contents_url(api: &str, repo: &str, path: &str) -> String {
    format!("{api}/repos/{repo}/contents/{}", encode_path(path))
}

/// ブランチ作成の JSON。
pub fn ref_create_body(branch: &str, sha: &str) -> Value {
    json!({ "ref": format!("refs/heads/{branch}"), "sha": sha })
}

/// contents PUT の JSON。`sha` は既存ファイル更新時のみ付ける。
pub fn contents_put_body(
    message: &str,
    content_b64: &str,
    branch: &str,
    sha: Option<&str>,
) -> Value {
    let mut v = json!({
        "message": message,
        "content": content_b64,
        "branch": branch,
    });
    if let Some(sha) = sha {
        v["sha"] = Value::String(sha.to_string());
    }
    v
}

/// PR 作成の JSON。
pub fn pr_create_body(title: &str, head: &str, base: &str, body: &str) -> Value {
    json!({
        "title": title,
        "head": head,
        "base": base,
        "body": body,
    })
}

/// git/ref 応答（`object.sha`）または contents 応答（トップレベル `sha`）から SHA を取り出す。
fn extract_sha(body: &str) -> Option<String> {
    let v: Value = serde_json::from_str(body).ok()?;
    v.get("object")
        .and_then(|o| o.get("sha"))
        .and_then(Value::as_str)
        .map(str::to_string)
        .or_else(|| v.get("sha").and_then(Value::as_str).map(str::to_string))
}

fn extract_str(body: &str, key: &str) -> Option<String> {
    let v: Value = serde_json::from_str(body).ok()?;
    v.get(key).and_then(Value::as_str).map(str::to_string)
}

fn extract_u64(body: &str, key: &str) -> Option<u64> {
    let v: Value = serde_json::from_str(body).ok()?;
    v.get(key).and_then(Value::as_u64)
}

fn summarize(body: &str) -> String {
    let one_line = body.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut end = one_line.len().min(MAX_ERROR_BODY).min(MAX_DETAIL_LINE);
    while end > 0 && !one_line.is_char_boundary(end) {
        end -= 1;
    }
    one_line[..end].to_string()
}

/// URL パス用: 予約されうる文字を percent-encode する（`/` は残す）。
fn encode_path(s: &str) -> String {
    percent_encode(s, true)
}

/// ref / クエリ用: `refs/heads/x` のようなスラッシュは残す。
fn encode_ref(s: &str) -> String {
    percent_encode(s, true)
}

fn percent_encode(s: &str, keep_slash: bool) -> String {
    let mut out = String::with_capacity(s.len());
    for &b in s.as_bytes() {
        let unreserved = b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~');
        if unreserved || (keep_slash && b == b'/') {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use protocol::RemoteFile;
    use std::io::{BufRead, BufReader, Read, Write};
    use std::net::{SocketAddr, TcpListener, TcpStream};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};
    use std::thread::JoinHandle;

    #[derive(Debug, Clone)]
    struct RecordedRequest {
        method: String,
        path: String,
        body: String,
    }

    /// テスト用の最小 HTTP サーバー。1 接続 1 リクエストで応答し、`Connection: close` を返す。
    struct MockServer {
        base_url: String,
        requests: Arc<Mutex<Vec<RecordedRequest>>>,
        _thread: JoinHandle<()>,
    }

    impl MockServer {
        fn start<F>(handler: F) -> Self
        where
            F: Fn(&RecordedRequest, usize) -> (u16, &'static str) + Send + Sync + 'static,
        {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let addr: SocketAddr = listener.local_addr().unwrap();
            let requests = Arc::new(Mutex::new(Vec::new()));
            let recorded = Arc::clone(&requests);
            let thread = std::thread::spawn(move || {
                let mut index = 0usize;
                for stream in listener.incoming() {
                    let Ok(mut stream) = stream else { break };
                    let Some(req) = read_request(&mut stream) else {
                        continue;
                    };
                    let (status, body) = handler(&req, index);
                    index += 1;
                    recorded.lock().unwrap().push(req);
                    let response = format!(
                        "HTTP/1.1 {status} {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                        reason_phrase(status),
                        body.len(),
                        body
                    );
                    let _ = stream.write_all(response.as_bytes());
                    let _ = stream.flush();
                }
            });
            Self {
                base_url: format!("http://{}", addr),
                requests,
                _thread: thread,
            }
        }

        fn requests(&self) -> Vec<RecordedRequest> {
            self.requests.lock().unwrap().clone()
        }
    }

    fn read_request(stream: &mut TcpStream) -> Option<RecordedRequest> {
        let mut reader = BufReader::new(stream.try_clone().ok()?);
        let mut line = String::new();
        if reader.read_line(&mut line).ok()? == 0 {
            return None;
        }
        let mut parts = line.split_whitespace();
        let method = parts.next()?.to_string();
        let path = parts.next()?.to_string();
        let mut content_length = 0usize;
        loop {
            let mut header = String::new();
            if reader.read_line(&mut header).ok()? == 0 {
                break;
            }
            if header == "\r\n" || header == "\n" {
                break;
            }
            let lower = header.to_ascii_lowercase();
            if let Some(value) = lower.strip_prefix("content-length:") {
                content_length = value.trim().parse().unwrap_or(0);
            }
        }
        let mut body = vec![0u8; content_length];
        if content_length > 0 {
            reader.read_exact(&mut body).ok()?;
        }
        Some(RecordedRequest {
            method,
            path,
            body: String::from_utf8_lossy(&body).into_owned(),
        })
    }

    fn reason_phrase(status: u16) -> &'static str {
        match status {
            200 => "OK",
            201 => "Created",
            422 => "Unprocessable Entity",
            _ => "Status",
        }
    }

    fn spec() -> RemoteSpec {
        RemoteSpec {
            repo: "owner/repo".to_string(),
            base_branch: "main".to_string(),
            branch: "ai-office/x".to_string(),
            title: "テスト PR".to_string(),
            body: "本文".to_string(),
            files: vec![RemoteFile {
                path: "docs/hello.md".to_string(),
                content: "hello".to_string(),
            }],
            token: "tok".to_string(),
        }
    }

    fn noop() -> impl FnMut(u8, &str) {
        |_p: u8, _m: &str| {}
    }

    #[test]
    fn body_builders_are_exact() {
        assert_eq!(
            ref_create_body("ai-office/x", "SHA1"),
            json!({"ref": "refs/heads/ai-office/x", "sha": "SHA1"})
        );
        assert_eq!(
            contents_put_body("m", "aGVsbG8=", "ai-office/x", None),
            json!({"message": "m", "content": "aGVsbG8=", "branch": "ai-office/x"})
        );
        assert_eq!(
            contents_put_body("m", "aGVsbG8=", "ai-office/x", Some("FILESHA")),
            json!({"message": "m", "content": "aGVsbG8=", "branch": "ai-office/x", "sha": "FILESHA"})
        );
        assert_eq!(
            pr_create_body("t", "head", "base", "b"),
            json!({"title": "t", "head": "head", "base": "base", "body": "b"})
        );
        assert_eq!(
            contents_url("https://api.github.com", "owner/repo", "docs/hello.md"),
            "https://api.github.com/repos/owner/repo/contents/docs/hello.md"
        );
        assert_eq!(
            base_ref_url("https://api.github.com", "owner/repo", "main"),
            "https://api.github.com/repos/owner/repo/git/ref/heads/main"
        );
    }

    #[test]
    fn missing_token_fails() {
        let mut s = spec();
        s.token = String::new();
        let env = RemoteEnv {
            api_base: "http://127.0.0.1:1".to_string(),
        };
        let res = execute_remote(&s, &env, &mut noop());
        assert_eq!(res.status, "failed");
        assert!(res.summary.contains("token 未設定"));
    }

    #[test]
    fn remote_creates_branch_pushes_files_and_opens_pr() {
        let server =
            MockServer::start(|req, _idx| match (req.method.as_str(), req.path.as_str()) {
                ("GET", "/repos/owner/repo/git/ref/heads/main") => {
                    (200, r#"{"object":{"sha":"BASESHA"}}"#)
                }
                ("POST", "/repos/owner/repo/git/refs") => {
                    (201, r#"{"ref":"refs/heads/ai-office/x"}"#)
                }
                ("PUT", "/repos/owner/repo/contents/docs/hello.md") => {
                    (201, r#"{"content":{"path":"docs/hello.md"}}"#)
                }
                ("POST", "/repos/owner/repo/pulls") => (
                    201,
                    r#"{"number":7,"html_url":"https://github.com/owner/repo/pull/7"}"#,
                ),
                _ => (404, r#"{"message":"not found"}"#),
            });

        let env = RemoteEnv {
            api_base: server.base_url.clone(),
        };
        let res = execute_remote(&spec(), &env, &mut noop());
        assert_eq!(res.status, "done", "detail: {}", res.detail);
        assert_eq!(
            res.summary,
            "PR を作成しました: https://github.com/owner/repo/pull/7"
        );
        assert_eq!(res.artifacts.len(), 1);
        assert_eq!(res.artifacts[0].path, "docs/hello.md");
        assert_eq!(res.artifacts[0].bytes, 5);

        let reqs = server.requests();
        assert_eq!(reqs.len(), 4);
        assert_eq!(reqs[0].method, "GET");
        assert_eq!(reqs[0].path, "/repos/owner/repo/git/ref/heads/main");
        assert_eq!(reqs[1].method, "POST");
        assert_eq!(reqs[1].path, "/repos/owner/repo/git/refs");
        // ureq は pretty-printed JSON を送るので、本文はパースして検証する。
        let ref_body: Value = serde_json::from_str(&reqs[1].body).unwrap();
        assert_eq!(ref_body["ref"], "refs/heads/ai-office/x");
        assert_eq!(ref_body["sha"], "BASESHA");
        assert_eq!(reqs[2].method, "PUT");
        assert_eq!(reqs[2].path, "/repos/owner/repo/contents/docs/hello.md");
        let put_body: Value = serde_json::from_str(&reqs[2].body).unwrap();
        // "hello" の base64 は aGVsbG8=。
        assert_eq!(put_body["content"], "aGVsbG8=");
        assert_eq!(put_body["branch"], "ai-office/x");
        assert_eq!(reqs[3].method, "POST");
        assert_eq!(reqs[3].path, "/repos/owner/repo/pulls");
        let pr_body: Value = serde_json::from_str(&reqs[3].body).unwrap();
        assert_eq!(pr_body["head"], "ai-office/x");
        assert_eq!(pr_body["base"], "main");
        assert_eq!(pr_body["title"], "テスト PR");
    }

    #[test]
    fn remote_existing_file_is_updated_with_sha() {
        let put_count = Arc::new(AtomicUsize::new(0));
        let puts = Arc::clone(&put_count);
        let server =
            MockServer::start(
                move |req, _idx| match (req.method.as_str(), req.path.as_str()) {
                    ("GET", "/repos/owner/repo/git/ref/heads/main") => {
                        (200, r#"{"object":{"sha":"BASESHA"}}"#)
                    }
                    ("POST", "/repos/owner/repo/git/refs") => {
                        (422, r#"{"message":"Reference already exists"}"#)
                    }
                    ("PUT", "/repos/owner/repo/contents/docs/hello.md") => {
                        let n = puts.fetch_add(1, Ordering::SeqCst);
                        if n == 0 {
                            (422, r#"{"message":"sha wasn't supplied"}"#)
                        } else {
                            (200, r#"{"content":{"path":"docs/hello.md"}}"#)
                        }
                    }
                    ("GET", "/repos/owner/repo/contents/docs/hello.md?ref=ai-office/x") => {
                        (200, r#"{"sha":"FILESHA"}"#)
                    }
                    ("POST", "/repos/owner/repo/pulls") => (
                        201,
                        r#"{"number":8,"html_url":"https://github.com/owner/repo/pull/8"}"#,
                    ),
                    _ => (404, r#"{"message":"not found"}"#),
                },
            );

        let env = RemoteEnv {
            api_base: server.base_url.clone(),
        };
        let res = execute_remote(&spec(), &env, &mut noop());
        assert_eq!(res.status, "done", "detail: {}", res.detail);
        assert_eq!(
            res.summary,
            "PR を作成しました: https://github.com/owner/repo/pull/8"
        );

        let reqs = server.requests();
        // GET ref, POST refs(422), PUT(422), GET contents, PUT(sha), POST pulls
        assert_eq!(reqs.len(), 6);
        assert_eq!(reqs[3].method, "GET");
        assert_eq!(
            reqs[3].path,
            "/repos/owner/repo/contents/docs/hello.md?ref=ai-office/x"
        );
        let retry_body: Value = serde_json::from_str(&reqs[4].body).unwrap();
        assert_eq!(retry_body["sha"], "FILESHA");
    }

    #[test]
    fn remote_prefers_object_sha_over_top_level() {
        assert_eq!(
            extract_sha(r#"{"object":{"sha":"A"}}"#).as_deref(),
            Some("A")
        );
        assert_eq!(extract_sha(r#"{"sha":"B"}"#).as_deref(), Some("B"));
    }
}
