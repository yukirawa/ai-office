package api

// llm_test.go は LLM 診断エンドポイント（/api/llm, /api/llm/ping）を検証する。
//
// 設計書 §10 の「LLMモック：internal/llm/mock.go で API 呼ばずにテスト」に従い、
// 実ネットワークには出ない。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/llm"
)

// newLLMTestServer は LLM 診断エンドポイント専用の小さなテストサーバを作る。
// store / presence / economy はこのハンドラでは触らないので渡さない。
func newLLMTestServer(t *testing.T, cfg *config.Config, client llm.Client) *httptest.Server {
	t.Helper()
	srv := New(Deps{Cfg: cfg, LLM: client})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestLLMInfo(t *testing.T) {
	cases := []struct {
		name           string
		provider       string
		withClient     bool
		wantMock       bool
		wantConfigured bool
	}{
		{"anthropic でクライアントあり", "anthropic", true, false, true},
		{"anthropic でクライアントなし", "anthropic", false, false, false},
		{"mock はクライアントなしでも configured", "mock", false, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var client llm.Client
			if tc.withClient {
				client = llm.NewMock("pong")
			}
			cfg := &config.Config{
				LLMProvider:  tc.provider,
				LLMModel:     "claude-3-5-haiku-latest",
				LLMTimeout:   2 * time.Minute,
				LLMMaxTokens: 1024,
			}
			ts := newLLMTestServer(t, cfg, client)

			resp, err := http.Get(ts.URL + "/api/llm")
			if err != nil {
				t.Fatalf("GET /api/llm: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			var got struct {
				Provider   string `json:"provider"`
				Model      string `json:"model"`
				MaxTokens  int    `json:"max_tokens"`
				Timeout    string `json:"timeout"`
				Configured bool   `json:"configured"`
				Mock       bool   `json:"mock"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if got.Provider != tc.provider {
				t.Errorf("provider = %q, want %q", got.Provider, tc.provider)
			}
			if got.Model != "claude-3-5-haiku-latest" {
				t.Errorf("model = %q, want claude-3-5-haiku-latest", got.Model)
			}
			if got.MaxTokens != 1024 {
				t.Errorf("max_tokens = %d, want 1024", got.MaxTokens)
			}
			if got.Timeout != "2m0s" {
				t.Errorf("timeout = %q, want 2m0s", got.Timeout)
			}
			if got.Mock != tc.wantMock {
				t.Errorf("mock = %v, want %v", got.Mock, tc.wantMock)
			}
			if got.Configured != tc.wantConfigured {
				t.Errorf("configured = %v, want %v", got.Configured, tc.wantConfigured)
			}
		})
	}
}

func TestLLMPingWithMock(t *testing.T) {
	mock := llm.NewMock("pong")
	cfg := &config.Config{
		LLMProvider:  "mock",
		LLMModel:     "claude-3-5-haiku-latest",
		LLMTimeout:   120 * time.Second,
		LLMMaxTokens: 1024,
	}
	ts := newLLMTestServer(t, cfg, mock)

	resp, err := http.Post(ts.URL+"/api/llm/ping", "application/json",
		bytes.NewBufferString(`{"message":"ping"}`))
	if err != nil {
		t.Fatalf("POST /api/llm/ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		OK           bool   `json:"ok"`
		Model        string `json:"model"`
		Text         string `json:"text"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("ok = false, want true")
	}
	if got.Text != "pong" {
		t.Errorf("text = %q, want pong", got.Text)
	}
	if got.Model == "" {
		t.Errorf("model が空です")
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Chat 呼び出し回数 = %d, want 1", len(reqs))
	}
	if reqs[0].Model != cfg.LLMModel {
		t.Errorf("request.Model = %q, want %q", reqs[0].Model, cfg.LLMModel)
	}
	if len(reqs[0].Messages) != 1 || reqs[0].Messages[0].Content != "ping" {
		t.Errorf("request.Messages = %+v", reqs[0].Messages)
	}
	if reqs[0].MaxTokens != pingMaxTokens {
		t.Errorf("request.MaxTokens = %d, want %d", reqs[0].MaxTokens, pingMaxTokens)
	}
}

func TestLLMPingDefaultMessage(t *testing.T) {
	mock := llm.NewMock("pong")
	ts := newLLMTestServer(t, &config.Config{LLMProvider: "mock", LLMModel: "m"}, mock)

	// body 無しでも既定プロンプトで呼べること。
	resp, err := http.Post(ts.URL+"/api/llm/ping", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/llm/ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Chat 呼び出し回数 = %d, want 1", len(reqs))
	}
	if len(reqs[0].Messages) != 1 || reqs[0].Messages[0].Content != defaultLLMPingPrompt {
		t.Errorf("既定メッセージが使われていません: %+v", reqs[0].Messages)
	}
}

func TestLLMPingNoClient(t *testing.T) {
	ts := newLLMTestServer(t, &config.Config{LLMProvider: "anthropic", LLMModel: "m"}, nil)

	resp, err := http.Post(ts.URL+"/api/llm/ping", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/llm/ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OK {
		t.Errorf("ok = true, want false")
	}
	if got.Error == "" {
		t.Errorf("error が空です")
	}
}

// failingLLMClient は Chat が常にエラーを返す Client（502 経路の検証用）。
type failingLLMClient struct{ err error }

func (f failingLLMClient) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, f.err
}

func TestLLMPingError(t *testing.T) {
	client := failingLLMClient{err: errors.New("401 unauthorized")}
	ts := newLLMTestServer(t, &config.Config{LLMProvider: "anthropic", LLMModel: "m"}, client)

	resp, err := http.Post(ts.URL+"/api/llm/ping", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/llm/ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OK {
		t.Errorf("ok = true, want false")
	}
	if !strings.Contains(got.Error, "401") {
		t.Errorf("error = %q, want 401 を含む", got.Error)
	}
}
