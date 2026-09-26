//! bubblewrap サンドボックス（設計書 §7.3, §13.5）。
//!
//! `exec` アクションは `OFFICE_SANDBOX=bwrap`（既定）のとき `bwrap` の下で実行する。
//! 引数ベクタの組み立ては純関数 [`bwrap_args`] に切り出してあり、`bwrap` が
//! インストールされていなくてもテストできる。

use std::path::{Path, PathBuf};

/// サンドボックス方式。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SandboxMode {
    /// bubblewrap で隔離する（既定）。
    Bwrap,
    /// 隔離しない。workspace のパス検査のみ行う。
    None,
}

impl SandboxMode {
    /// `OFFICE_SANDBOX` を解釈する。
    ///
    /// 戻り値の `Option<String>` は警告メッセージ（未知値のとき）で、通常は `None`。
    pub fn from_env_value(value: &str) -> (Self, Option<String>) {
        match value.trim() {
            "" | "bwrap" => (Self::Bwrap, None),
            "none" => (Self::None, None),
            other => (
                Self::Bwrap,
                Some(format!(
                    "unknown OFFICE_SANDBOX={other:?}; falling back to bwrap"
                )),
            ),
        }
    }
}

/// read-only で bind する標準パス（存在するもののみ）。
///
/// 設計書の「`/usr` と `/lib*` と `/bin`」を具体化したもの。存在しないパスを
/// そのまま `--ro-bind` に渡すと `bwrap` が失敗するためフィルタする。
pub fn default_ro_binds() -> Vec<PathBuf> {
    ["/usr", "/lib", "/lib64", "/bin", "/sbin"]
        .into_iter()
        .map(PathBuf::from)
        .filter(|p| p.exists())
        .collect()
}

/// `bwrap` の引数ベクタを組み立てる（純関数）。
///
/// - workspace を read-write で bind（`--bind <ws> <ws>`）
/// - `ro_binds` を read-only で bind（`--ro-bind <p> <p>`）
/// - `--dev /dev`、`--proc /proc`
/// - `--unshare-net` でネットワークを遮断
/// - `--die-with-parent` で親の終了に追従
/// - `--chdir <cwd>` で作業ディレクトリを固定
///
/// 実際の起動コマンドは `bwrap <戻り値...>` となる。
pub fn bwrap_args(
    workspace: &Path,
    ro_binds: &[PathBuf],
    cwd: &Path,
    cmd: &str,
    args: &[String],
) -> Vec<String> {
    let mut v = Vec::new();
    for p in ro_binds {
        let s = p.display().to_string();
        v.push("--ro-bind".to_string());
        v.push(s.clone());
        v.push(s);
    }
    v.push("--dev".to_string());
    v.push("/dev".to_string());
    v.push("--proc".to_string());
    v.push("/proc".to_string());
    v.push("--bind".to_string());
    let ws = workspace.display().to_string();
    v.push(ws.clone());
    v.push(ws);
    v.push("--unshare-net".to_string());
    v.push("--die-with-parent".to_string());
    v.push("--chdir".to_string());
    v.push(cwd.display().to_string());
    v.push("--".to_string());
    v.push(cmd.to_string());
    v.extend(args.iter().cloned());
    v
}

/// `PATH` から実行ファイルを探す（`bwrap` の存在確認用）。
pub fn find_in_path(name: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|dir| dir.join(name))
        .find(|candidate| candidate.is_file())
}

/// `bwrap` が実行可能なら `true`。
pub fn bwrap_available() -> bool {
    find_in_path("bwrap").is_some()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sandbox_mode_parsing() {
        assert_eq!(SandboxMode::from_env_value("").0, SandboxMode::Bwrap);
        assert_eq!(SandboxMode::from_env_value("bwrap").0, SandboxMode::Bwrap);
        assert_eq!(SandboxMode::from_env_value("none").0, SandboxMode::None);
        let (mode, warning) = SandboxMode::from_env_value("docker");
        assert_eq!(mode, SandboxMode::Bwrap);
        assert!(warning.is_some());
    }

    #[test]
    fn bwrap_args_built_as_specified() {
        let workspace = PathBuf::from("/home/office/ws");
        let ro = vec![PathBuf::from("/usr"), PathBuf::from("/lib")];
        let cwd = PathBuf::from("/home/office/ws/sub");
        let args = vec!["-c".to_string(), "echo hi".to_string()];
        let got = bwrap_args(&workspace, &ro, &cwd, "sh", &args);
        let want: Vec<String> = [
            "--ro-bind",
            "/usr",
            "/usr",
            "--ro-bind",
            "/lib",
            "/lib",
            "--dev",
            "/dev",
            "--proc",
            "/proc",
            "--bind",
            "/home/office/ws",
            "/home/office/ws",
            "--unshare-net",
            "--die-with-parent",
            "--chdir",
            "/home/office/ws/sub",
            "--",
            "sh",
            "-c",
            "echo hi",
        ]
        .into_iter()
        .map(str::to_string)
        .collect();
        assert_eq!(got, want);
    }

    #[test]
    fn default_ro_binds_exist_on_disk() {
        // 存在するパスだけが返る（存在チェックのフィルタが効いている）。
        for p in default_ro_binds() {
            assert!(p.exists(), "{} should exist", p.display());
        }
    }
}
