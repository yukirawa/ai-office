// Package gh は GitHub 連携（設計書 §2 の gh、Phase 3）の入り口。
//
// このパッケージは他パッケージに依存しない standalone な実装で、次の 2 つを提供する。
//
//   - GitHub App の installation token 発行と、それを用いた Pull Request 作成
//     （Config / GitHubClient / Client）
//   - GitHub webhook（/webhook/github）の署名検証・パース・ハンドラ（webhook.go）
//
// 設計書 §9 のとおり、GitHub App の権限は repo, pull_requests, issues のみを想定する。
//
// 実装メモ: GitHub REST API への呼び出しは標準ライブラリの net/http で直接行っている。
// go-github/v66 は go.sum に推移的依存 (go-querystring) のエントリが無くビルドできないため、
// go.mod/go.sum を変更しない方針に合わせて raw HTTP を採用した。
package gh

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// defaultBaseURL は GitHub.com の REST API ベース URL。
	defaultBaseURL = "https://api.github.com"
	// defaultBaseBranch は PRRequest.BaseBranch が空のときに使う base ブランチ。
	defaultBaseBranch = "main"
	// userAgent は GitHub API に送る User-Agent（設計で固定）。
	userAgent = "ai-office"
	// acceptHeader は GitHub が推奨する Accept ヘッダ。
	acceptHeader = "application/vnd.github+json"
	// httpTimeout は 1 リクエストあたりの上限。ctx によるキャンセルは別途尊重される。
	httpTimeout = 30 * time.Second
	// tokenRefreshMargin は期限切れ間際にキャッシュを使い続けないための余裕。
	tokenRefreshMargin = time.Minute
	// maxResponseBody は読み込むレスポンス本文の上限。
	maxResponseBody = 1 << 20
	// maxErrorBody はエラーメッセージに埋め込む本文の最大バイト数。
	maxErrorBody = 512
)

// Config は GitHub App 連携に必要な設定。呼び出し側（internal/config など）が
// 環境変数をここにマッピングする。gh パッケージは config に依存しない。
type Config struct {
	AppID          int64  // GitHub App ID
	InstallationID int64  // installation ID
	PrivateKeyPEM  string // RSA private key in PEM (PKCS#1 or PKCS#8)
	BaseURL        string // API base, default "https://api.github.com"; tests override this
}

// File は 1 ファイル分のコミット内容。
type File struct {
	Path    string // repo-relative path
	Content string // UTF-8 text content
}

// PRRequest は Pull Request 作成の入力。Repo は "owner/name" 形式。
type PRRequest struct {
	Repo       string // "owner/name"
	BaseBranch string // default "main" when empty
	Branch     string // new branch name
	Title      string
	Body       string
	Files      []File
}

// PRResult は Pull Request 作成の結果。
type PRResult struct {
	Number    int
	HTMLURL   string
	Branch    string
	CommitSHA string
}

// Client は gh パッケージの公開 API。他パッケージはこのインターフェース越しに使う。
type Client interface {
	InstallationToken(ctx context.Context) (string, error)
	CreatePullRequest(ctx context.Context, req PRRequest) (PRResult, error)
}

// GitHubClient は Client の実装。installation token をメモリにキャッシュする。
type GitHubClient struct {
	cfg        Config
	baseURL    *url.URL
	key        *rsa.PrivateKey
	httpClient *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

var _ Client = (*GitHubClient)(nil)

// New は Config を検証して GitHubClient を組み立てる。
// AppID / InstallationID / PrivateKeyPEM が欠けている場合や、PEM が不正な場合は error を返す。
func New(cfg Config) (*GitHubClient, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("gh: AppID is required")
	}
	if cfg.InstallationID <= 0 {
		return nil, errors.New("gh: InstallationID is required")
	}
	if strings.TrimSpace(cfg.PrivateKeyPEM) == "" {
		return nil, errors.New("gh: PrivateKeyPEM is required")
	}
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &GitHubClient{
		cfg:        cfg,
		baseURL:    baseURL,
		key:        key,
		httpClient: &http.Client{Timeout: httpTimeout},
	}, nil
}

// InstallationToken は GitHub App の installation access token を発行する。
//
// RS256 の JWT（iss=AppID, iat=now-60s, exp=now+9min）で認証し、
// POST {BaseURL}/app/installations/{InstallationID}/access_tokens を叩く。
// 取得したトークンは期限付きでメモリにキャッシュし、有効なうちは再取得しない。
func (c *GitHubClient) InstallationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.token != "" && now.Before(c.tokenExpiry.Add(-tokenRefreshMargin)) {
		return c.token, nil
	}

	claims := jwt.MapClaims{
		// GitHub は iss を文字列で要求する。
		"iss": strconv.FormatInt(c.cfg.AppID, 10),
		"iat": jwt.NewNumericDate(now.Add(-60 * time.Second)),
		"exp": jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("gh: sign app jwt: %w", err)
	}

	rel := "app/installations/" + strconv.FormatInt(c.cfg.InstallationID, 10) + "/access_tokens"
	status, body, err := c.do(ctx, http.MethodPost, rel, signed, nil)
	if err != nil {
		return "", fmt.Errorf("gh: request installation token: %w", err)
	}
	if status < 200 || status > 299 {
		return "", fmt.Errorf("gh: installation token: %s", statusError(status, body))
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("gh: decode installation token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("gh: installation token: response contains no token")
	}

	expiry := now.Add(time.Hour)
	if out.ExpiresAt != "" {
		if t, perr := time.Parse(time.RFC3339, out.ExpiresAt); perr == nil {
			expiry = t
		}
	}
	c.token = out.Token
	c.tokenExpiry = expiry
	return c.token, nil
}

// CreatePullRequest は base ブランチから新しいブランチを切り、Files をコミットして PR を作る。
//
// 手順:
//  1. GET  .../git/ref/heads/{base}    で base のコミット SHA を取得
//  2. POST .../git/refs                で新ブランチを作成（422 = 既存は成功扱い）
//  3. PUT  .../contents/{path}         を各ファイル分（422 = 既存なら SHA を取得して再 PUT）
//  4. POST .../pulls                   で PR 作成
func (c *GitHubClient) CreatePullRequest(ctx context.Context, req PRRequest) (PRResult, error) {
	owner, repo, err := splitRepo(req.Repo)
	if err != nil {
		return PRResult{}, err
	}
	if req.Branch == "" {
		return PRResult{}, errors.New("gh: branch is required")
	}
	if len(req.Files) == 0 {
		return PRResult{}, errors.New("gh: files is required")
	}
	base := req.BaseBranch
	if base == "" {
		base = defaultBaseBranch
	}

	token, err := c.InstallationToken(ctx)
	if err != nil {
		return PRResult{}, err
	}

	repoPath := "repos/" + escapeSegments(owner) + "/" + escapeSegments(repo)

	// 1. base ブランチの先頭コミット SHA。
	status, body, err := c.do(ctx, http.MethodGet, repoPath+"/git/ref/heads/"+escapeSegments(base), token, nil)
	if err != nil {
		return PRResult{}, fmt.Errorf("gh: get base ref %q: %w", base, err)
	}
	if status != http.StatusOK {
		return PRResult{}, fmt.Errorf("gh: get base ref %q: %s", base, statusError(status, body))
	}
	var refResp struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &refResp); err != nil {
		return PRResult{}, fmt.Errorf("gh: get base ref %q: decode response: %w", base, err)
	}
	if refResp.Object.SHA == "" {
		return PRResult{}, fmt.Errorf("gh: get base ref %q: response contains no sha", base)
	}
	baseSHA := refResp.Object.SHA

	// 2. 新ブランチ。既存（422）ならそのまま進む。
	createRefBody := map[string]string{"ref": "refs/heads/" + req.Branch, "sha": baseSHA}
	status, body, err = c.do(ctx, http.MethodPost, repoPath+"/git/refs", token, createRefBody)
	if err != nil {
		return PRResult{}, fmt.Errorf("gh: create branch %q: %w", req.Branch, err)
	}
	if status != http.StatusCreated && status != http.StatusOK && status != http.StatusUnprocessableEntity {
		return PRResult{}, fmt.Errorf("gh: create branch %q: %s", req.Branch, statusError(status, body))
	}

	// 3. 各ファイルをコミット。
	commitSHA := ""
	for _, f := range req.Files {
		if f.Path == "" {
			return PRResult{}, errors.New("gh: file path is required")
		}
		contentPath := repoPath + "/contents/" + escapeSegments(f.Path)
		payload := map[string]string{
			"message": commitMessage(req, f),
			"content": base64.StdEncoding.EncodeToString([]byte(f.Content)),
			"branch":  req.Branch,
		}
		status, body, err = c.do(ctx, http.MethodPut, contentPath, token, payload)
		if err != nil {
			return PRResult{}, fmt.Errorf("gh: put contents %q: %w", f.Path, err)
		}
		if status == http.StatusUnprocessableEntity {
			// 既存ファイル: 現在の blob SHA を取得してから更新する。
			sha, gerr := c.getContentSHA(ctx, contentPath, token, req.Branch)
			if gerr != nil {
				return PRResult{}, fmt.Errorf("gh: get content sha %q: %w", f.Path, gerr)
			}
			payload["sha"] = sha
			status, body, err = c.do(ctx, http.MethodPut, contentPath, token, payload)
			if err != nil {
				return PRResult{}, fmt.Errorf("gh: put contents %q: %w", f.Path, err)
			}
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return PRResult{}, fmt.Errorf("gh: put contents %q: %s", f.Path, statusError(status, body))
		}
		var putResp struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &putResp); err == nil && putResp.Commit.SHA != "" {
			commitSHA = putResp.Commit.SHA
		}
	}

	// 4. Pull Request 作成。
	prPayload := map[string]string{
		"title": req.Title,
		"head":  req.Branch,
		"base":  base,
		"body":  req.Body,
	}
	status, body, err = c.do(ctx, http.MethodPost, repoPath+"/pulls", token, prPayload)
	if err != nil {
		return PRResult{}, fmt.Errorf("gh: create pull request: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return PRResult{}, fmt.Errorf("gh: create pull request: %s", statusError(status, body))
	}
	var prResp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &prResp); err != nil {
		return PRResult{}, fmt.Errorf("gh: create pull request: decode response: %w", err)
	}

	return PRResult{
		Number:    prResp.Number,
		HTMLURL:   prResp.HTMLURL,
		Branch:    req.Branch,
		CommitSHA: commitSHA,
	}, nil
}

// getContentSHA は contents API で既存ファイルの blob SHA を取得する。
func (c *GitHubClient) getContentSHA(ctx context.Context, contentPath, token, ref string) (string, error) {
	status, body, err := c.do(ctx, http.MethodGet, contentPath+"?ref="+url.QueryEscape(ref), token, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", statusError(status, body)
	}
	var got struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if got.SHA == "" {
		return "", errors.New("response contains no sha")
	}
	return got.SHA, nil
}

// do は REST API 呼び出しを行う。relPath は BaseURL からの相対パス。
// token が空でなければ Bearer 認証を付ける。payload は JSON エンコードして送る。
func (c *GitHubClient) do(ctx context.Context, method, relPath, token string, payload any) (int, []byte, error) {
	endpoint := c.baseURL.String() + relPath

	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("User-Agent", userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp.StatusCode, body, nil
}

// commitMessage は各ファイルのコミットメッセージを決める。
func commitMessage(req PRRequest, f File) string {
	if req.Title != "" {
		return req.Title
	}
	return "Update " + f.Path
}

// splitRepo は "owner/name" を分解する。
func splitRepo(repo string) (owner, name string, err error) {
	if repo == "" {
		return "", "", errors.New("gh: repo is required")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("gh: invalid repo %q: want \"owner/name\"", repo)
	}
	return parts[0], parts[1], nil
}

// normalizeBaseURL は BaseURL を検証し、パス連結のために末尾スラッシュを付与する。
func normalizeBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		raw = defaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gh: invalid BaseURL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gh: invalid BaseURL %q: scheme and host are required", raw)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u, nil
}

// parseRSAPrivateKey は PKCS#1 / PKCS#8 の PEM を RSA 秘密鍵として読む。
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("gh: PrivateKeyPEM: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gh: PrivateKeyPEM: not a valid PKCS#1/PKCS#8 key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("gh: PrivateKeyPEM: want RSA key, got %T", parsed)
	}
	return key, nil
}

// escapeSegments はパスを "/" 区切りでセグメントごとに URL エスケープする。
// ブランチ名やファイルパスに含まれる "/" は区切りとして保持する。
func escapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// statusError は非 2xx レスポンスを、ステータスと本文（切り詰め）を含む error にする。
func statusError(status int, body []byte) error {
	return fmt.Errorf("status %d: %s", status, truncate(body))
}

// truncate はエラーメッセージ用に本文を短く切る。
func truncate(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "...(truncated)"
	}
	return s
}
