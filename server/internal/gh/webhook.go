package gh

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// signatureHeader は GitHub が HMAC-SHA256 署名を載せるヘッダ（設計書 §13.6）。
	signatureHeader = "X-Hub-Signature-256"
	// eventHeader はイベント種別が入るヘッダ。
	eventHeader = "X-GitHub-Event"
	// maxWebhookBody は読み込む webhook 本文の上限（1 MiB）。
	maxWebhookBody = 1 << 20
)

// VerifyWebhookSignature は X-Hub-Signature-256 (sha256=<hex>) を HMAC-SHA256 で検証する。
// secret が空なら nil を返す（開発用に検証スキップ）。署名が不正なら error。
func VerifyWebhookSignature(secret string, body []byte, signature string) error {
	if secret == "" {
		return nil
	}
	if signature == "" {
		return errors.New("gh: webhook signature is missing")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return fmt.Errorf("gh: unsupported webhook signature format %q", signature)
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil {
		return fmt.Errorf("gh: invalid webhook signature encoding: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errors.New("gh: webhook signature mismatch")
	}
	return nil
}

// WebhookEvent は webhook 本文から取り出した最小限の情報。
type WebhookEvent struct {
	Kind   string // "issues" | "pull_request" | "ping" | ...
	Action string // "opened" | "closed" | ...
	Repo   string // "owner/name"
	Number int
	Title  string
	Body   string
	Sender string
}

type webhookRepository struct {
	FullName string `json:"full_name"`
}

type webhookSender struct {
	Login string `json:"login"`
}

type issuesPayload struct {
	Action     string            `json:"action"`
	Repository webhookRepository `json:"repository"`
	Issue      struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	} `json:"issue"`
	Sender webhookSender `json:"sender"`
}

type pullRequestPayload struct {
	Action      string            `json:"action"`
	Repository  webhookRepository `json:"repository"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	} `json:"pull_request"`
	Sender webhookSender `json:"sender"`
}

// ParseWebhook は X-GitHub-Event ヘッダと本文から最小限の情報を取り出す。
// issues / pull_request は型付きで、それ以外（ping・未知イベント）は Kind のみを返す。
// 未知イベントでも JSON として壊れていれば error を返す。
func ParseWebhook(eventHeader string, body []byte) (WebhookEvent, error) {
	ev := WebhookEvent{Kind: eventHeader}
	if len(strings.TrimSpace(string(body))) == 0 {
		return ev, nil
	}

	switch eventHeader {
	case "issues":
		var p issuesPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return WebhookEvent{}, fmt.Errorf("gh: parse issues webhook: %w", err)
		}
		ev.Action = p.Action
		ev.Repo = p.Repository.FullName
		ev.Number = p.Issue.Number
		ev.Title = p.Issue.Title
		ev.Body = p.Issue.Body
		ev.Sender = p.Sender.Login
	case "pull_request":
		var p pullRequestPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return WebhookEvent{}, fmt.Errorf("gh: parse pull_request webhook: %w", err)
		}
		ev.Action = p.Action
		ev.Repo = p.Repository.FullName
		ev.Number = p.PullRequest.Number
		ev.Title = p.PullRequest.Title
		ev.Body = p.PullRequest.Body
		ev.Sender = p.Sender.Login
	default:
		// ping や未知イベントは Kind のみ。ただし本文が JSON として壊れていないかだけ検証する。
		var raw json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return WebhookEvent{}, fmt.Errorf("gh: parse %s webhook: %w", eventHeader, err)
		}
	}
	return ev, nil
}

// Handler は webhook を受けるハンドラを返す。
// 署名を検証し、問題なければ onEvent を呼ぶ。ping / 未知イベントも onEvent に渡す（呼び出し側で判断）。
// 署名不正は 401、本文が壊れていれば 400、成功は 202 を返す。
func Handler(secret string, onEvent func(ctx context.Context, ev WebhookEvent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			writeWebhookError(w, http.StatusBadRequest, "failed to read request body")
			return
		}

		if err := VerifyWebhookSignature(secret, body, r.Header.Get(signatureHeader)); err != nil {
			writeWebhookError(w, http.StatusUnauthorized, err.Error())
			return
		}

		ev, err := ParseWebhook(r.Header.Get(eventHeader), body)
		if err != nil {
			writeWebhookError(w, http.StatusBadRequest, err.Error())
			return
		}

		if onEvent != nil {
			onEvent(r.Context(), ev)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
	}
}

func writeWebhookError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
