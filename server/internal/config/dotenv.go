// dotenv.go はプロジェクトルートの .env を読み込む小さなローダー。
//
// 依存を増やさないため自前で実装する。仕様:
//   - KEY=VALUE 形式。`export ` 前置、前後の空白、囲み引用符（' / "）に対応。
//   - 空行と # 始まりの行はコメントとして無視する。
//   - 既に設定済みの環境変数は上書きしない（プロセスに渡された env を優先）。
//   - 探す候補は OFFICE_ENV_FILE（指定時）、".env"、"../.env"。
//     `server/` から起動した場合とリポジトリ直下から起動した場合の両方に対応する。
package config

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// dotEnvCandidates は .env を探す候補パスを優先順で返す。
func dotEnvCandidates() []string {
	if p := strings.TrimSpace(os.Getenv("OFFICE_ENV_FILE")); p != "" {
		return []string{p}
	}
	return []string{".env", filepath.Join("..", ".env")}
}

// loadDotEnv は候補の .env のうち最初に見つかったものを読み込み、未設定の環境変数に適用する。
// ファイルが無ければ何もしない（エラーにしない）。
func loadDotEnv() {
	for _, path := range dotEnvCandidates() {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		vals := parseDotEnv(f)
		_ = f.Close()
		applyDotEnv(vals)
		return
	}
}

// parseDotEnv は .env の内容を KEY->VALUE に解析する（純粋関数・テスト対象）。
func parseDotEnv(r io.Reader) map[string]string {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := unquote(strings.TrimSpace(line[eq+1:]))
		if key == "" {
			continue
		}
		out[key] = val
	}
	return out
}

// unquote は値が引用符で囲まれていれば外す。
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// applyDotEnv は未設定のキーだけを環境変数へ設定する（既存 env を優先）。
func applyDotEnv(vals map[string]string) {
	for k, v := range vals {
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		_ = os.Setenv(k, v)
	}
}
