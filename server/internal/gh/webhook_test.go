package gh

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	valid := signBody(secret, body)

	if err := VerifyWebhookSignature(secret, body, valid); err != nil {
		t.Errorf("valid signature: unexpected error: %v", err)
	}
	if err := VerifyWebhookSignature(secret, []byte(`{"action":"closed"}`), valid); err == nil {
		t.Error("tampered body: expected error")
	}
	tampered := "sha256=" + strings.Repeat("0", 64)
	if err := VerifyWebhookSignature(secret, body, tampered); err == nil {
		t.Error("tampered signature: expected error")
	}
	if err := VerifyWebhookSignature(secret, body, ""); err == nil {
		t.Error("missing signature: expected error")
	}
	if err := VerifyWebhookSignature(secret, body, "sha1=deadbeef"); err == nil {
		t.Error("wrong prefix: expected error")
	}
	if err := VerifyWebhookSignature(secret, body, "sha256=zzzz"); err == nil {
		t.Error("bad hex: expected error")
	}

	// secret が空なら検証スキップ。
	if err := VerifyWebhookSignature("", body, ""); err != nil {
		t.Errorf("empty secret should skip verification: %v", err)
	}
	if err := VerifyWebhookSignature("", body, "garbage"); err != nil {
		t.Errorf("empty secret should skip verification: %v", err)
	}
}

func TestParseWebhook(t *testing.T) {
	t.Run("issues", func(t *testing.T) {
		body := []byte(`{
			"action":"opened",
			"repository":{"full_name":"owner/name"},
			"issue":{"number":12,"title":"Bug","body":"details"},
			"sender":{"login":"alice"}
		}`)
		ev, err := ParseWebhook("issues", body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		want := WebhookEvent{Kind: "issues", Action: "opened", Repo: "owner/name", Number: 12, Title: "Bug", Body: "details", Sender: "alice"}
		if ev != want {
			t.Errorf("event = %+v, want %+v", ev, want)
		}
	})

	t.Run("pull_request", func(t *testing.T) {
		body := []byte(`{
			"action":"closed",
			"repository":{"full_name":"owner/name"},
			"pull_request":{"number":34,"title":"Feature","body":"pr body"},
			"sender":{"login":"bob"}
		}`)
		ev, err := ParseWebhook("pull_request", body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		want := WebhookEvent{Kind: "pull_request", Action: "closed", Repo: "owner/name", Number: 34, Title: "Feature", Body: "pr body", Sender: "bob"}
		if ev != want {
			t.Errorf("event = %+v, want %+v", ev, want)
		}
	})

	t.Run("ping", func(t *testing.T) {
		ev, err := ParseWebhook("ping", []byte(`{"zen":"design"}`))
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if ev.Kind != "ping" || ev.Action != "" || ev.Number != 0 {
			t.Errorf("event = %+v, want Kind only", ev)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		ev, err := ParseWebhook("release", []byte(`{"action":"published"}`))
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if ev.Kind != "release" {
			t.Errorf("Kind = %q, want release", ev.Kind)
		}
	})

	t.Run("malformed known", func(t *testing.T) {
		if _, err := ParseWebhook("issues", []byte(`{not json`)); err == nil {
			t.Error("expected error for malformed issues body")
		}
	})

	t.Run("malformed unknown", func(t *testing.T) {
		if _, err := ParseWebhook("release", []byte(`{not json`)); err == nil {
			t.Error("expected error for malformed unknown body")
		}
	})

	t.Run("empty body", func(t *testing.T) {
		ev, err := ParseWebhook("ping", nil)
		if err != nil || ev.Kind != "ping" {
			t.Errorf("empty body: ev=%+v err=%v", ev, err)
		}
	})
}

func TestHandler(t *testing.T) {
	secret := "hook-secret"
	body := []byte(`{
		"action":"opened",
		"repository":{"full_name":"owner/name"},
		"issue":{"number":5,"title":"hello","body":"world"},
		"sender":{"login":"carol"}
	}`)

	t.Run("valid signature", func(t *testing.T) {
		var called int32
		var got WebhookEvent
		h := Handler(secret, func(ctx context.Context, ev WebhookEvent) {
			atomic.AddInt32(&called, 1)
			got = ev
		})

		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
		req.Header.Set(signatureHeader, signBody(secret, body))
		req.Header.Set(eventHeader, "issues")
		rec := httptest.NewRecorder()
		h(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if atomic.LoadInt32(&called) != 1 {
			t.Fatalf("onEvent called %d times, want 1", called)
		}
		if got.Kind != "issues" || got.Action != "opened" || got.Number != 5 || got.Title != "hello" || got.Body != "world" || got.Repo != "owner/name" || got.Sender != "carol" {
			t.Errorf("event = %+v", got)
		}
	})

	t.Run("invalid signature", func(t *testing.T) {
		var called int32
		h := Handler(secret, func(ctx context.Context, ev WebhookEvent) { atomic.AddInt32(&called, 1) })

		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
		req.Header.Set(signatureHeader, "sha256="+strings.Repeat("a", 64))
		req.Header.Set(eventHeader, "issues")
		rec := httptest.NewRecorder()
		h(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if called != 0 {
			t.Fatal("onEvent must not be called on invalid signature")
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		var called int32
		h := Handler(secret, func(ctx context.Context, ev WebhookEvent) { atomic.AddInt32(&called, 1) })

		bad := []byte(`{not json`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(bad)))
		req.Header.Set(signatureHeader, signBody(secret, bad))
		req.Header.Set(eventHeader, "issues")
		rec := httptest.NewRecorder()
		h(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if called != 0 {
			t.Fatal("onEvent must not be called on malformed body")
		}
	})

	t.Run("empty secret skips verification", func(t *testing.T) {
		var called int32
		h := Handler("", func(ctx context.Context, ev WebhookEvent) { atomic.AddInt32(&called, 1) })

		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(`{"zen":"x"}`))
		req.Header.Set(eventHeader, "ping")
		rec := httptest.NewRecorder()
		h(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if atomic.LoadInt32(&called) != 1 {
			t.Fatal("onEvent should be called")
		}
	})

	t.Run("nil onEvent", func(t *testing.T) {
		h := Handler(secret, nil)
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(`{"zen":"x"}`))
		req.Header.Set(signatureHeader, signBody(secret, []byte(`{"zen":"x"}`)))
		req.Header.Set(eventHeader, "ping")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
	})
}
