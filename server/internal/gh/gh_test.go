package gh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// --- テストヘルパ ---

func testPrivateKeyPEM(t *testing.T, pkcs8 bool) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	var der []byte
	blockType := "RSA PRIVATE KEY"
	if pkcs8 {
		der, err = x509.MarshalPKCS8PrivateKey(key)
		blockType = "PRIVATE KEY"
	} else {
		der = x509.MarshalPKCS1PrivateKey(key)
	}
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recorder) add(req *http.Request, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.Query(),
		Header: req.Header.Clone(),
		Body:   body,
	})
}

func (r *recorder) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- New ---

func TestNewValidation(t *testing.T) {
	_, pkcs1 := testPrivateKeyPEM(t, false)

	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing AppID", Config{InstallationID: 1, PrivateKeyPEM: pkcs1}},
		{"missing InstallationID", Config{AppID: 1, PrivateKeyPEM: pkcs1}},
		{"missing PrivateKeyPEM", Config{AppID: 1, InstallationID: 1}},
		{"invalid PEM", Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: "not a pem"}},
		{"bad BaseURL", Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: pkcs1, BaseURL: "://bad"}},
	}
	for _, tc := range cases {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}

	if _, err := New(Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: pkcs1}); err != nil {
		t.Errorf("valid PKCS#1 config: unexpected error: %v", err)
	}
	_, pkcs8 := testPrivateKeyPEM(t, true)
	if _, err := New(Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: pkcs8}); err != nil {
		t.Errorf("valid PKCS#8 config: unexpected error: %v", err)
	}
}

// --- InstallationToken ---

func TestInstallationTokenJWTAndCache(t *testing.T) {
	key, pemStr := testPrivateKeyPEM(t, false)

	var (
		mu           sync.Mutex
		hits         int
		method, path string
		authz        string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		method, path, authz = r.Method, r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		writeJSON(w, http.StatusCreated, `{"token":"tok-abc","expires_at":"2099-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	c, err := New(Config{AppID: 123, InstallationID: 456, PrivateKeyPEM: pemStr, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := c.InstallationToken(context.Background())
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if got != "tok-abc" {
		t.Fatalf("token = %q, want tok-abc", got)
	}

	// 2 回目はキャッシュから返り、サーバには当たらない。
	if got2, err := c.InstallationToken(context.Background()); err != nil || got2 != "tok-abc" {
		t.Fatalf("cached InstallationToken = (%q, %v), want (tok-abc, nil)", got2, err)
	}
	mu.Lock()
	n := hits
	mu.Unlock()
	if n != 1 {
		t.Fatalf("server hits = %d, want 1 (token should be cached)", n)
	}

	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if path != "/app/installations/456/access_tokens" {
		t.Errorf("path = %s, want /app/installations/456/access_tokens", path)
	}
	if !strings.HasPrefix(authz, "Bearer ") {
		t.Fatalf("Authorization = %q, want Bearer <jwt>", authz)
	}

	tokenStr := strings.TrimPrefix(authz, "Bearer ")
	parsed, err := jwt.Parse(tokenStr, func(tk *jwt.Token) (interface{}, error) {
		return &key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		t.Fatalf("jwt.Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("jwt is not valid")
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims type = %T, want jwt.MapClaims", parsed.Claims)
	}
	if iss, _ := claims["iss"].(string); iss != "123" {
		t.Errorf("iss = %v, want \"123\"", claims["iss"])
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if diff := exp - iat; diff < 599 || diff > 601 {
		t.Errorf("exp-iat = %v, want ~600s (iat=now-60s, exp=now+9min)", diff)
	}
}

func TestInstallationTokenHTTPError(t *testing.T) {
	_, pemStr := testPrivateKeyPEM(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	c, err := New(Config{AppID: 1, InstallationID: 2, PrivateKeyPEM: pemStr, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.InstallationToken(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error = %v, want status and body", err)
	}
}

// --- CreatePullRequest ---

func TestCreatePullRequestSequence(t *testing.T) {
	_, pemStr := testPrivateKeyPEM(t, false)
	rec := &recorder{}
	var bPuts int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r, body)

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/99/access_tokens":
			writeJSON(w, http.StatusCreated, `{"token":"tok-pr","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/name/git/ref/heads/main":
			writeJSON(w, http.StatusOK, `{"ref":"refs/heads/main","object":{"type":"commit","sha":"basesha"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/name/git/refs":
			// 既存ブランチ: 422 は成功扱いになる。
			writeJSON(w, http.StatusUnprocessableEntity, `{"message":"Reference already exists"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/name/contents/a.txt":
			writeJSON(w, http.StatusCreated, `{"content":{"path":"a.txt"},"commit":{"sha":"commit-a"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/name/contents/b.txt":
			bPuts++
			if bPuts == 1 {
				writeJSON(w, http.StatusUnprocessableEntity, `{"message":"sha wasn't supplied"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"content":{"path":"b.txt"},"commit":{"sha":"commit-b"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/name/contents/b.txt":
			writeJSON(w, http.StatusOK, `{"type":"file","path":"b.txt","sha":"blob-b"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/name/pulls":
			writeJSON(w, http.StatusCreated, `{"number":7,"html_url":"https://github.com/owner/name/pull/7"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New(Config{AppID: 1, InstallationID: 99, PrivateKeyPEM: pemStr, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := c.CreatePullRequest(context.Background(), PRRequest{
		Repo:       "owner/name",
		BaseBranch: "", // 既定 main
		Branch:     "feat",
		Title:      "Add files",
		Body:       "PR body",
		Files: []File{
			{Path: "a.txt", Content: "hello"},
			{Path: "b.txt", Content: "world"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if res.Number != 7 {
		t.Errorf("Number = %d, want 7", res.Number)
	}
	if res.HTMLURL != "https://github.com/owner/name/pull/7" {
		t.Errorf("HTMLURL = %q", res.HTMLURL)
	}
	if res.Branch != "feat" {
		t.Errorf("Branch = %q, want feat", res.Branch)
	}
	if res.CommitSHA != "commit-b" {
		t.Errorf("CommitSHA = %q, want commit-b (last commit)", res.CommitSHA)
	}

	reqs := rec.snapshot()
	want := []struct{ method, path string }{
		{"POST", "/app/installations/99/access_tokens"},
		{"GET", "/repos/owner/name/git/ref/heads/main"},
		{"POST", "/repos/owner/name/git/refs"},
		{"PUT", "/repos/owner/name/contents/a.txt"},
		{"PUT", "/repos/owner/name/contents/b.txt"},
		{"GET", "/repos/owner/name/contents/b.txt"},
		{"PUT", "/repos/owner/name/contents/b.txt"},
		{"POST", "/repos/owner/name/pulls"},
	}
	if len(reqs) != len(want) {
		t.Fatalf("recorded %d requests, want %d: %+v", len(reqs), len(want), paths(reqs))
	}
	for i, w := range want {
		if reqs[i].Method != w.method || reqs[i].Path != w.path {
			t.Errorf("reqs[%d] = %s %s, want %s %s", i, reqs[i].Method, reqs[i].Path, w.method, w.path)
		}
	}

	// installation token 以外は installation token で認証されている。
	for i := 1; i < len(reqs); i++ {
		if got := reqs[i].Header.Get("Authorization"); got != "Bearer tok-pr" {
			t.Errorf("reqs[%d] Authorization = %q, want Bearer tok-pr", i, got)
		}
	}

	// create ref ボディ。
	var refBody struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(reqs[2].Body, &refBody); err != nil {
		t.Fatalf("unmarshal create-ref body: %v", err)
	}
	if refBody.Ref != "refs/heads/feat" || refBody.SHA != "basesha" {
		t.Errorf("create-ref body = %+v, want refs/heads/feat @ basesha", refBody)
	}

	// a.txt のコンテンツは base64 化されている。
	contentBody := decodeContentBody(t, reqs[3].Body)
	if contentBody.Branch != "feat" {
		t.Errorf("contents branch = %q, want feat", contentBody.Branch)
	}
	if contentBody.Message != "Add files" {
		t.Errorf("contents message = %q, want Add files", contentBody.Message)
	}
	decoded, err := base64.StdEncoding.DecodeString(contentBody.Content)
	if err != nil {
		t.Fatalf("base64 decode content: %v", err)
	}
	if string(decoded) != "hello" {
		t.Errorf("decoded content = %q, want hello", string(decoded))
	}

	// 2 回目の b.txt は SHA 付きで再 PUT され、GET は ref=feat。
	if got := reqs[5].Query.Get("ref"); got != "feat" {
		t.Errorf("get contents ref = %q, want feat", got)
	}
	retryBody := decodeContentBody(t, reqs[6].Body)
	if retryBody.SHA != "blob-b" {
		t.Errorf("retry sha = %q, want blob-b", retryBody.SHA)
	}

	// PR ボディ。
	var prBody struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(reqs[7].Body, &prBody); err != nil {
		t.Fatalf("unmarshal pr body: %v", err)
	}
	if prBody.Title != "Add files" || prBody.Head != "feat" || prBody.Base != "main" || prBody.Body != "PR body" {
		t.Errorf("pr body = %+v", prBody)
	}
}

func TestCreatePullRequestValidation(t *testing.T) {
	_, pemStr := testPrivateKeyPEM(t, false)
	c, err := New(Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: pemStr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		req  PRRequest
	}{
		{"empty repo", PRRequest{Branch: "b", Files: []File{{Path: "a", Content: "x"}}}},
		{"bad repo", PRRequest{Repo: "noslash", Branch: "b", Files: []File{{Path: "a", Content: "x"}}}},
		{"empty branch", PRRequest{Repo: "o/r", Files: []File{{Path: "a", Content: "x"}}}},
		{"empty files", PRRequest{Repo: "o/r", Branch: "b"}},
	}
	for _, tc := range cases {
		if _, err := c.CreatePullRequest(context.Background(), tc.req); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

type contentBody struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
}

func decodeContentBody(t *testing.T, body []byte) contentBody {
	t.Helper()
	var cb contentBody
	if err := json.Unmarshal(body, &cb); err != nil {
		t.Fatalf("unmarshal contents body: %v", err)
	}
	return cb
}

func paths(reqs []recordedRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Method + " " + r.Path
	}
	return out
}

// ctx キャンセルが InstallationToken に伝わることの確認。
func TestInstallationTokenRespectsContext(t *testing.T) {
	_, pemStr := testPrivateKeyPEM(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(w, http.StatusCreated, `{"token":"late"}`)
	}))
	defer srv.Close()

	c, err := New(Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: pemStr, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.InstallationToken(ctx); err == nil {
		t.Fatal("expected context deadline error")
	}
}
