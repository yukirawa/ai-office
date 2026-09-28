//! local モードのタスク実行（設計書 §7.3, §13.5, Phase 2.1）。
//!
//! `actions[]` を順に実行する。すべてのパスは workspace ルートに解決し、
//! ルート外へ脱出するもの（`..` によるトラバーサル、ルート外の絶対パス、
//! ルート外を指すシンボリックリンク）は拒否する。
//!
//! サンドボックスの適用範囲: `read` / `write` / `list` はパス検査で workspace 内に
//! 限定できるため **インプロセス**で実行する。任意コマンドを走らせる `exec` だけを
//! `bwrap` の下で実行する（`OFFICE_SANDBOX=bwrap` かつ bwrap 利用可能なとき）。

use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};

use protocol::{Artifact, TaskAssignPayload};

use crate::sandbox::{self, SandboxMode};

/// `read` で detail に載せる最大バイト数。
const MAX_READ_BYTES: usize = 4096;
/// `exec` の stdout / stderr を detail に載せるときの最大文字数。
const MAX_EXEC_OUTPUT: usize = 4096;
/// detail 全体の上限。超過分は切り詰める。
const MAX_DETAIL_LEN: usize = 16 * 1024;

/// local モードの実行環境。
#[derive(Debug, Clone)]
pub struct LocalEnv {
    /// 作業ディレクトリ。タスクの `workspace` 指定があればそのルート、
    /// なければ `OFFICE_WORKSPACE`。local モードでは必須。
    pub root: Option<PathBuf>,
    /// `OFFICE_ALLOW_EXEC=1` のときのみ `exec` を許可する。
    pub allow_exec: bool,
    /// 解決済みのサンドボックス方式（bwrap 不在時は `None` に落としてある）。
    pub sandbox: SandboxMode,
}

/// 実行結果。`status` は `"done"` | `"failed"`。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LocalResult {
    pub status: &'static str,
    pub summary: String,
    pub detail: String,
    pub artifacts: Vec<Artifact>,
}

/// `actions[]` を順番に実行する。
///
/// 個々のアクションの失敗では中断せず、最後まで実行して結果を集約する。
/// これは自律的な探索（存在しないファイルの read など）を許容するため。
/// ひとつも成功せず全滅した場合のみ `failed` を返す。
/// `on_progress` は各アクションの開始前に `(percent, message)` で呼ばれる。
pub fn execute_local(
    payload: &TaskAssignPayload,
    env: &LocalEnv,
    cancelled: &AtomicBool,
    on_progress: &mut dyn FnMut(u8, &str),
) -> LocalResult {
    let Some(root_raw) = env.root.as_ref().filter(|p| !p.as_os_str().is_empty()) else {
        return failed("OFFICE_WORKSPACE が未設定です（local モードには必須）");
    };
    let root = match root_raw.canonicalize() {
        Ok(p) => p,
        Err(err) => {
            return failed(format!(
                "OFFICE_WORKSPACE を解決できません ({}): {err}",
                root_raw.display()
            ));
        }
    };

    let total = payload.actions.len();
    if total == 0 {
        return LocalResult {
            status: "done",
            summary: "0 アクションを実行しました".to_string(),
            detail: String::new(),
            artifacts: Vec::new(),
        };
    }

    let mut log_lines: Vec<String> = Vec::new();
    let mut artifacts: Vec<Artifact> = Vec::new();
    let mut errors: Vec<String> = Vec::new();
    let mut ok_count = 0usize;

    for (i, action) in payload.actions.iter().enumerate() {
        if cancelled.load(Ordering::Relaxed) {
            log_lines.push(format!("[{}/{}] キャンセルを検出", i + 1, total));
            return failed_with("タスクはキャンセルされました", log_lines, artifacts);
        }

        let percent = ((i * 100) / total).min(100) as u8;
        let op = action.op.trim().to_string();
        on_progress(percent, &format!("action {}/{}: {op}", i + 1, total));
        let step = format!("[{}/{}] {op}", i + 1, total);

        let outcome = match op.as_str() {
            "read" => do_read(&root, action.path.trim(), &step, &mut log_lines),
            "write" => do_write(
                &root,
                action.path.trim(),
                &action.content,
                &step,
                &mut log_lines,
                &mut artifacts,
            ),
            "list" => do_list(&root, action.path.trim(), &step, &mut log_lines),
            "exec" => do_exec(&root, action, env, &step, &mut log_lines),
            other => Err(format!("未知の op です: {other:?}")),
        };

        if let Err(reason) = outcome {
            // 1 つの失敗では全体を止めない（探索での「無いファイルの read」などを許容）。
            log_lines.push(format!("WARN: {reason}"));
            errors.push(reason);
        } else {
            ok_count += 1;
        }
    }

    // ひとつも成功しなかった場合は失敗とする（例: 唯一の write が失敗）。
    if ok_count == 0 && !errors.is_empty() {
        return failed_with(errors.join("; "), log_lines, artifacts);
    }

    let summary = if errors.is_empty() {
        if let Some(first) = artifacts.first() {
            format!("{total} アクションを実行しました（write {}）", first.path)
        } else {
            format!("{total} アクションを実行しました")
        }
    } else {
        format!(
            "{total} アクション中 {ok_count} 件成功、{} 件失敗",
            errors.len()
        )
    };
    LocalResult {
        status: "done",
        summary,
        detail: truncate(&log_lines.join("\n"), MAX_DETAIL_LEN),
        artifacts,
    }
}

/// 実行前検査: `requested` を workspace ルート配下の実パスへ解決する。
///
/// `root` は canonicalize 済みであること。ルート外へ出る場合は `Err`。
pub fn resolve_within(root: &Path, requested: &str) -> Result<PathBuf, String> {
    if requested.is_empty() {
        return Err("path が空です".to_string());
    }
    let req = Path::new(requested);
    let joined = if req.is_absolute() {
        req.to_path_buf()
    } else {
        root.join(req)
    };
    let normalized = normalize_lexical(&joined);
    if !normalized.starts_with(root) {
        return Err(format!("workspace 外のパスは拒否されました: {requested}"));
    }

    // シンボリックリンク脱出のベストエフォート検査。
    // 実在する最も近い祖先を canonicalize し、ルート配下か確認する。
    let mut probe = normalized.as_path();
    while !probe.exists() {
        match probe.parent() {
            Some(parent) if parent.starts_with(root) => probe = parent,
            _ => break,
        }
    }
    if probe.exists() {
        let canon = probe
            .canonicalize()
            .map_err(|err| format!("パスを解決できません {requested}: {err}"))?;
        if !canon.starts_with(root) {
            return Err(format!(
                "シンボリックリンクが workspace 外を指しています: {requested}"
            ));
        }
    }
    Ok(normalized)
}

/// タスクごとの作業先を、許可ルートに照らして決定する。
///
/// - `requested` が空なら `base`（`OFFICE_WORKSPACE`）を従来どおり使う。
/// - 非空なら canonicalize（未作成なら実在する最も近い祖先を canonicalize）した上で、
///   `allowed` + `base` のいずれかの配下にあることを確認する。
///   許可ルートが空のときは `base` のみ許可する。
/// - 許可外なら `Err`。決定したルートが無ければ `create_dir_all` する。
///
/// 許可判定は canonicalize 済みで行うため、シンボリックリンクでの脱出も防げる。
pub fn resolve_task_root(
    requested: &str,
    base: Option<&PathBuf>,
    allowed: &[PathBuf],
) -> Result<PathBuf, String> {
    // 許可ルートを canonicalize した集合を作る。base（workspace）も常に含める。
    let mut allowed_canon: Vec<PathBuf> = Vec::new();
    let mut push_allowed = |candidate: &Path| {
        if candidate.as_os_str().is_empty() {
            return;
        }
        if let Ok(c) = canonicalize_lenient(candidate)
            && !allowed_canon.contains(&c)
        {
            allowed_canon.push(c);
        }
    };
    if let Some(base) = base {
        push_allowed(base);
    }
    for root in allowed {
        push_allowed(root);
    }

    let requested = requested.trim();
    if requested.is_empty() {
        let Some(base) = base.filter(|p| !p.as_os_str().is_empty()) else {
            return Err("OFFICE_WORKSPACE が未設定です（local モードには必須）".to_string());
        };
        let root = canonicalize_lenient(base)?;
        ensure_dir(&root)?;
        return Ok(root);
    }

    let root = canonicalize_lenient(Path::new(requested))?;
    if !allowed_canon.iter().any(|a| root.starts_with(a)) {
        return Err(format!(
            "作業先が許可されていません: {requested}（OFFICE_ALLOWED_ROOTS に追加してください）"
        ));
    }
    ensure_dir(&root)?;
    Ok(root)
}

/// 作業ディレクトリを必要なら作成する。
fn ensure_dir(root: &Path) -> Result<(), String> {
    if root.is_dir() {
        return Ok(());
    }
    std::fs::create_dir_all(root)
        .map_err(|err| format!("作業先を作成できません {}: {err}", root.display()))
}

/// `path` を可能な範囲で canonicalize する。
///
/// 実在すればそのまま canonicalize し、未作成なら実在する最も近い祖先を
/// canonicalize してから残りの要素を字句的に連結する。これにより、まだ無い
/// 作業先でもシンボリックリンク脱出を判定できる。
pub fn canonicalize_lenient(path: &Path) -> Result<PathBuf, String> {
    let normalized = normalize_lexical(path);
    if let Ok(canon) = normalized.canonicalize() {
        return Ok(canon);
    }
    let mut suffix: Vec<std::ffi::OsString> = Vec::new();
    let mut probe = normalized;
    loop {
        let Some(parent) = probe.parent().map(Path::to_path_buf) else {
            return Err(format!("パスを解決できません: {}", path.display()));
        };
        if let Some(name) = probe.file_name() {
            suffix.push(name.to_os_string());
        }
        match parent.canonicalize() {
            Ok(canon) => {
                let mut result = canon;
                for part in suffix.iter().rev() {
                    result.push(part);
                }
                return Ok(result);
            }
            Err(_) if !parent.as_os_str().is_empty() => probe = parent,
            Err(err) => return Err(format!("パスを解決できません {}: {err}", path.display())),
        }
    }
}

/// `exec` の実コマンド（実行ファイルと引数）を決める。
///
/// LLM は `mkdir -p a && echo hi > a/f` のようなシェル風の 1 行コマンドを
/// `cmd` にそのまま入れ、`args` を空にしてくることが多い。そのままだと実行
/// ファイル名として扱われて失敗するため、`args` が空で `cmd` に空白かシェル
/// メタ文字を含むときだけ `sh -c <cmd>` に正規化する。`args` があるときは
/// 従来どおりそのまま使う。
fn normalize_exec(cmd: &str, args: &[String]) -> (String, Vec<String>) {
    if args.is_empty() && cmd.chars().any(is_shell_meta) {
        ("sh".to_string(), vec!["-c".to_string(), cmd.to_string()])
    } else {
        (cmd.to_string(), args.to_vec())
    }
}

/// シェル経由を要する文字（空白・引用符・制御文字・展開）かどうか。
fn is_shell_meta(c: char) -> bool {
    c.is_whitespace()
        || matches!(
            c,
            '"' | '\'' | '|' | '&' | ';' | '<' | '>' | '(' | ')' | '$'
        )
}

/// `.` / `..` をファイルシステムに触れず字句的に解決する。
fn normalize_lexical(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for comp in path.components() {
        match comp {
            Component::Prefix(p) => out.push(p.as_os_str()),
            Component::RootDir => out.push(Component::RootDir.as_os_str()),
            Component::CurDir => {}
            Component::ParentDir => {
                // ルートより上には戻らない。
                out.pop();
            }
            Component::Normal(c) => out.push(c),
        }
    }
    out
}

fn do_read(
    root: &Path,
    requested: &str,
    step: &str,
    log_lines: &mut Vec<String>,
) -> Result<(), String> {
    let path = resolve_within(root, requested)?;
    let bytes = std::fs::read(&path).map_err(|err| format!("read 失敗 {requested}: {err}"))?;
    let truncated = bytes.len() > MAX_READ_BYTES;
    let slice = &bytes[..bytes.len().min(MAX_READ_BYTES)];
    log_lines.push(format!("{step} {}", display_rel(root, &path)));
    log_lines.push(format!(
        "  {} bytes{}",
        bytes.len(),
        if truncated {
            " (先頭 4096 バイトのみ表示)"
        } else {
            ""
        }
    ));
    let text = String::from_utf8_lossy(slice);
    for line in text.lines() {
        log_lines.push(format!("  {line}"));
    }
    Ok(())
}

fn do_write(
    root: &Path,
    requested: &str,
    content: &str,
    step: &str,
    log_lines: &mut Vec<String>,
    artifacts: &mut Vec<Artifact>,
) -> Result<(), String> {
    let path = resolve_within(root, requested)?;
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|err| format!("mkdir 失敗 {}: {err}", parent.display()))?;
    }
    std::fs::write(&path, content.as_bytes())
        .map_err(|err| format!("write 失敗 {requested}: {err}"))?;
    let bytes = content.len() as u64;
    let rel = display_rel(root, &path);
    log_lines.push(format!("{step} {rel} ({bytes} bytes)"));
    artifacts.push(Artifact { path: rel, bytes });
    Ok(())
}

fn do_list(
    root: &Path,
    requested: &str,
    step: &str,
    log_lines: &mut Vec<String>,
) -> Result<(), String> {
    let dir = if requested.is_empty() {
        root.to_path_buf()
    } else {
        resolve_within(root, requested)?
    };
    let entries = std::fs::read_dir(&dir).map_err(|err| format!("list 失敗 {requested}: {err}"))?;
    let mut names: Vec<String> = entries
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    log_lines.push(format!("{step} {}", display_rel(root, &dir)));
    if names.is_empty() {
        log_lines.push("  (空)".to_string());
    }
    for name in names {
        log_lines.push(format!("  {name}"));
    }
    Ok(())
}

fn do_exec(
    root: &Path,
    action: &protocol::Action,
    env: &LocalEnv,
    step: &str,
    log_lines: &mut Vec<String>,
) -> Result<(), String> {
    if !env.allow_exec {
        return Err("exec は無効です（OFFICE_ALLOW_EXEC=1 で許可されます）".to_string());
    }
    let cmd = action.cmd.trim();
    if cmd.is_empty() {
        return Err("exec の cmd が空です".to_string());
    }
    let cwd = if action.cwd.trim().is_empty() {
        root.to_path_buf()
    } else {
        resolve_within(root, action.cwd.trim())?
    };
    if !cwd.is_dir() {
        return Err(format!("cwd がディレクトリではありません: {}", action.cwd));
    }

    // シェル風の 1 行コマンドを `sh -c` に正規化してから bwrap / none 双方で使う。
    let (prog, args) = normalize_exec(cmd, &action.args);
    let mut command = match env.sandbox {
        SandboxMode::Bwrap => {
            let ro = sandbox::default_ro_binds();
            let mut c = std::process::Command::new("bwrap");
            c.args(sandbox::bwrap_args(root, &ro, &cwd, &prog, &args));
            c
        }
        SandboxMode::None => {
            let mut c = std::process::Command::new(&prog);
            c.args(&args);
            c.current_dir(&cwd);
            c
        }
    };

    log_lines.push(format!("{step} {prog} {}", args.join(" ")));
    let output = command
        .output()
        .map_err(|err| format!("exec 起動失敗 {cmd}: {err}"))?;

    push_output(
        log_lines,
        "stdout",
        &String::from_utf8_lossy(&output.stdout),
    );
    push_output(
        log_lines,
        "stderr",
        &String::from_utf8_lossy(&output.stderr),
    );
    let code = output
        .status
        .code()
        .map(|c| c.to_string())
        .unwrap_or_else(|| "signal".to_string());
    log_lines.push(format!("  exit: {code}"));

    if !output.status.success() {
        return Err(format!("exec が非ゼロ終了しました ({code}): {cmd}"));
    }
    Ok(())
}

fn push_output(log_lines: &mut Vec<String>, label: &str, text: &str) {
    if text.trim().is_empty() {
        return;
    }
    for line in truncate(text, MAX_EXEC_OUTPUT).lines() {
        log_lines.push(format!("  {label}: {line}"));
    }
}

/// `root` からの相対表示（`/` 区切り）。ルート自身は `.`。
fn display_rel(root: &Path, path: &Path) -> String {
    match path.strip_prefix(root) {
        Ok(rel) => {
            let s = rel.to_string_lossy().replace('\\', "/");
            if s.is_empty() { ".".to_string() } else { s }
        }
        Err(_) => path.display().to_string(),
    }
}

/// 文字境界を壊さずに `max` バイトまで切り詰める。
fn truncate(s: &str, max: usize) -> String {
    if s.len() <= max {
        return s.to_string();
    }
    let mut end = max;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}\n...(省略)", &s[..end])
}

fn failed(reason: impl Into<String>) -> LocalResult {
    let reason = reason.into();
    LocalResult {
        status: "failed",
        summary: reason.clone(),
        detail: reason,
        artifacts: Vec::new(),
    }
}

fn failed_with(
    reason: impl Into<String>,
    log_lines: Vec<String>,
    artifacts: Vec<Artifact>,
) -> LocalResult {
    LocalResult {
        status: "failed",
        summary: reason.into(),
        detail: truncate(&log_lines.join("\n"), MAX_DETAIL_LEN),
        artifacts,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use protocol::Action;
    use std::sync::atomic::AtomicU64;
    use std::time::{SystemTime, UNIX_EPOCH};

    static COUNTER: AtomicU64 = AtomicU64::new(0);

    /// テスト専用の一時ディレクトリ（外部クレートを使わない）。
    fn temp_dir(tag: &str) -> PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let seq = COUNTER.fetch_add(1, Ordering::Relaxed);
        let dir = std::env::temp_dir().join(format!(
            "ai-office-worker-test-{tag}-{}-{nanos}-{seq}",
            std::process::id()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn env_at(root: &Path) -> LocalEnv {
        LocalEnv {
            root: Some(root.to_path_buf()),
            allow_exec: false,
            sandbox: SandboxMode::None,
        }
    }

    fn noop_progress() -> impl FnMut(u8, &str) {
        |_p: u8, _m: &str| {}
    }

    fn payload(actions: Vec<Action>) -> TaskAssignPayload {
        TaskAssignPayload {
            mode: "local".to_string(),
            actions,
            ..Default::default()
        }
    }

    #[test]
    fn write_read_list_happy_path() {
        let dir = temp_dir("happy");
        let actions = vec![
            Action {
                op: "write".to_string(),
                path: "reports/x.md".to_string(),
                content: "hello\nworld".to_string(),
                ..Default::default()
            },
            Action {
                op: "read".to_string(),
                path: "reports/x.md".to_string(),
                ..Default::default()
            },
            Action {
                op: "list".to_string(),
                path: "reports".to_string(),
                ..Default::default()
            },
        ];
        let cancelled = AtomicBool::new(false);
        let res = execute_local(
            &payload(actions),
            &env_at(&dir),
            &cancelled,
            &mut noop_progress(),
        );

        assert_eq!(res.status, "done", "detail: {}", res.detail);
        assert_eq!(res.artifacts.len(), 1);
        assert_eq!(res.artifacts[0].path, "reports/x.md");
        assert_eq!(res.artifacts[0].bytes, "hello\nworld".len() as u64);
        assert!(res.summary.contains("3 アクション"));
        assert!(res.summary.contains("write reports/x.md"));
        assert!(res.detail.contains("hello"));
        assert!(res.detail.contains("x.md"));
        assert_eq!(
            std::fs::read_to_string(dir.join("reports/x.md")).unwrap(),
            "hello\nworld"
        );
    }

    #[test]
    fn missing_workspace_fails() {
        let cancelled = AtomicBool::new(false);
        let env = LocalEnv {
            root: None,
            allow_exec: false,
            sandbox: SandboxMode::None,
        };
        let res = execute_local(
            &payload(vec![Action {
                op: "list".to_string(),
                ..Default::default()
            }]),
            &env,
            &cancelled,
            &mut noop_progress(),
        );
        assert_eq!(res.status, "failed");
        assert!(res.summary.contains("OFFICE_WORKSPACE"));
    }

    #[test]
    fn path_traversal_is_rejected() {
        let dir = temp_dir("traversal");
        let outside = dir.parent().unwrap().join(format!(
            "ai-office-escaped-{}.txt",
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let actions = vec![Action {
            op: "write".to_string(),
            path: format!("../{}", outside.file_name().unwrap().to_string_lossy()),
            content: "pwned".to_string(),
            ..Default::default()
        }];
        let cancelled = AtomicBool::new(false);
        let res = execute_local(
            &payload(actions),
            &env_at(&dir),
            &cancelled,
            &mut noop_progress(),
        );
        assert_eq!(res.status, "failed");
        assert!(res.summary.contains("workspace 外"), "{}", res.summary);
        assert!(!outside.exists(), "escape file must not be created");
    }

    #[test]
    fn absolute_path_outside_root_is_rejected() {
        let dir = temp_dir("absolute");
        let res = resolve_within(&dir.canonicalize().unwrap(), "/etc/passwd");
        assert!(res.is_err());
    }

    #[test]
    fn lexical_dotdot_within_root_is_allowed() {
        let dir = temp_dir("dotdot").canonicalize().unwrap();
        let ok = resolve_within(&dir, "a/b/../c.txt").unwrap();
        assert_eq!(ok, dir.join("a/c.txt"));
    }

    #[cfg(unix)]
    #[test]
    fn symlink_escape_is_rejected() {
        let dir = temp_dir("symlink");
        let root = dir.canonicalize().unwrap();
        let outside = temp_dir("symlink-target");
        std::os::unix::fs::symlink(&outside, root.join("link")).unwrap();
        let res = resolve_within(&root, "link/secret.txt");
        assert!(res.is_err(), "symlink escape must be rejected");
    }

    #[test]
    fn exec_is_disabled_without_allow_exec() {
        let dir = temp_dir("execoff");
        let actions = vec![Action {
            op: "exec".to_string(),
            cmd: "echo".to_string(),
            args: vec!["hi".to_string()],
            ..Default::default()
        }];
        let cancelled = AtomicBool::new(false);
        let res = execute_local(
            &payload(actions),
            &env_at(&dir),
            &cancelled,
            &mut noop_progress(),
        );
        assert_eq!(res.status, "failed");
        assert!(res.summary.contains("OFFICE_ALLOW_EXEC"), "{}", res.summary);
    }

    #[cfg(unix)]
    #[test]
    fn exec_runs_when_allowed() {
        if !Path::new("/bin/sh").exists() {
            return;
        }
        let dir = temp_dir("execon");
        let actions = vec![Action {
            op: "exec".to_string(),
            cmd: "/bin/sh".to_string(),
            args: vec!["-c".to_string(), "echo hello-from-exec".to_string()],
            ..Default::default()
        }];
        let env = LocalEnv {
            root: Some(dir),
            allow_exec: true,
            sandbox: SandboxMode::None,
        };
        let cancelled = AtomicBool::new(false);
        let res = execute_local(&payload(actions), &env, &cancelled, &mut noop_progress());
        assert_eq!(res.status, "done", "detail: {}", res.detail);
        assert!(res.detail.contains("hello-from-exec"));
    }

    #[test]
    fn unknown_op_fails() {
        let dir = temp_dir("unknownop");
        let actions = vec![Action {
            op: "delete".to_string(),
            ..Default::default()
        }];
        let cancelled = AtomicBool::new(false);
        let res = execute_local(
            &payload(actions),
            &env_at(&dir),
            &cancelled,
            &mut noop_progress(),
        );
        assert_eq!(res.status, "failed");
        assert!(res.summary.contains("未知の op"));
    }

    #[test]
    fn resolve_task_root_allows_within_allowed_roots() {
        // 許可ルート内の（未作成の）サブディレクトリは採用され、作成される。
        let base = temp_dir("root-base-in");
        let allowed_root = temp_dir("root-allowed-in");
        let target = allowed_root.join("some-site");
        let got = resolve_task_root(
            target.to_str().unwrap(),
            Some(&base),
            &[allowed_root.clone()],
        )
        .expect("allowed root should be accepted");
        assert_eq!(got, target.canonicalize().unwrap());
        assert!(target.is_dir(), "missing root must be created");
    }

    #[test]
    fn resolve_task_root_rejects_outside_allowed_roots() {
        // 許可ルート外はエラー。
        let base = temp_dir("root-base-out");
        let allowed_root = temp_dir("root-allowed-out");
        let outside = temp_dir("root-outside");
        let target = outside.join("site");
        let err = resolve_task_root(target.to_str().unwrap(), Some(&base), &[allowed_root])
            .expect_err("outside root must be rejected");
        assert!(err.contains("許可されていません"), "{err}");
        assert!(err.contains("OFFICE_ALLOWED_ROOTS"), "{err}");
        assert!(!target.exists(), "rejected root must not be created");
    }

    #[test]
    fn resolve_task_root_base_workspace_is_always_allowed() {
        // allowed が空でも base は常に許可される。
        let base = temp_dir("root-base-always");
        let target = base.join("proj");
        let got = resolve_task_root(target.to_str().unwrap(), Some(&base), &[])
            .expect("base workspace must always be allowed");
        assert_eq!(got, target.canonicalize().unwrap());
    }

    #[test]
    fn resolve_task_root_empty_request_uses_base() {
        let base = temp_dir("root-empty");
        let got = resolve_task_root("", Some(&base), &[]).unwrap();
        assert_eq!(got, base.canonicalize().unwrap());
    }

    #[cfg(unix)]
    #[test]
    fn resolve_task_root_rejects_symlink_escape() {
        // 許可ルート内のシンボリックリンクが外を指す場合は拒否する。
        let base = temp_dir("root-base-sym");
        let allowed_root = temp_dir("root-sym-allowed");
        let outside = temp_dir("root-sym-outside");
        std::os::unix::fs::symlink(&outside, allowed_root.join("link")).unwrap();
        let target = allowed_root.join("link/secret");
        let err = resolve_task_root(target.to_str().unwrap(), Some(&base), &[allowed_root])
            .expect_err("symlink escape must be rejected");
        assert!(err.contains("許可されていません"), "{err}");
    }

    #[test]
    fn normalize_exec_rewrites_shell_style_cmd() {
        // args 空＋シェル風の 1 行コマンドは `sh -c` に正規化される。
        let (prog, args) = normalize_exec("mkdir -p a && echo hi > a/f", &[]);
        assert_eq!(prog, "sh");
        assert_eq!(
            args,
            vec!["-c".to_string(), "mkdir -p a && echo hi > a/f".to_string()]
        );
    }

    #[test]
    fn normalize_exec_keeps_explicit_args() {
        // args があるときは従来どおりそのまま使う。
        let args_in = vec!["-c".to_string(), "echo hi".to_string()];
        let (prog, args) = normalize_exec("/bin/sh", &args_in);
        assert_eq!(prog, "/bin/sh");
        assert_eq!(args, args_in);
    }

    #[test]
    fn normalize_exec_keeps_plain_command() {
        // シェルメタ文字を含まない単純なコマンドはそのまま。
        let (prog, args) = normalize_exec("ls", &[]);
        assert_eq!(prog, "ls");
        assert!(args.is_empty());
    }
}
