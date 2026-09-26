package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRetryDelay はバックオフおよび Retry-After の上限。
	maxRetryDelay = 30 * time.Second
	// maxErrorBodyRunes はエラーレスポンス本文をログ/エラーに含める際の上限文字数。
	maxErrorBodyRunes = 512
	// defaultMaxTokens はリクエストで MaxTokens が未指定（0 以下）の場合に使う値。
	defaultMaxTokens = 1024
)

// httpResult は 1 回の試行の結果。リトライ判定に必要な情報を含む。
type httpResult struct {
	resp       Response
	retryable  bool
	retryAfter time.Duration
	hasAfter   bool
	err        error
}

// retryableStatus はリトライすべき HTTP ステータスか。
// 429（レート制限）と 500/502/503/504（サーバ側の一時障害）のみ true。
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// computeRetryDelay は Retry-After（あれば優先、30s 上限）と指数バックオフから
// 次の試行までの待ち時間を返す。
//
// attempt はこれから行うリトライの 1 始まりの回数（1 = 最初のリトライ）で、
// バックオフは base * 2^(attempt-1) となる。Retry-After があれば base を無視する。
func computeRetryDelay(base time.Duration, attempt int, retryAfter time.Duration, hasRetryAfter bool) time.Duration {
	if hasRetryAfter {
		return capDelay(retryAfter)
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		if d >= maxRetryDelay {
			return maxRetryDelay
		}
		d *= 2
	}
	return capDelay(d)
}

// capDelay は待ち時間を [0, maxRetryDelay] に丸める。
func capDelay(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxRetryDelay {
		return maxRetryDelay
	}
	return d
}

// waitContext は d だけ待つ。待機中も ctx のキャンセルを監視し、
// キャンセルされたら ctx のエラーを返す。d <= 0 でも ctx を確認する。
func waitContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRetryAfter は Retry-After ヘッダを解釈する。
// 秒数または HTTP-date に対応し、値が存在すれば hasAfter=true を返す。
func parseRetryAfter(v string) (d time.Duration, hasAfter bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// transportError は http.Client.Do のエラーをリトライ可否つきに正規化する。
// ctx のキャンセル/期限切れはリトライせず、その他のトランスポートエラーは再試行する。
func transportError(ctx context.Context, err error) httpResult {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return httpResult{err: fmt.Errorf("llm: request failed: %w", ctxErr)}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return httpResult{err: fmt.Errorf("llm: request failed: %w", err)}
	}
	// トランスポート（ネットワーク）エラーはリトライ対象。
	return httpResult{retryable: true, err: fmt.Errorf("llm: request failed: %w", err)}
}

// apiStatusError はステータスコードと本文（切り詰め）に、よくある原因への
// 対処ヒントを付けたエラーを組み立てる。provider は "anthropic" / "deepseek" など。
func apiStatusError(provider string, status int, body []byte) error {
	msg := fmt.Sprintf("llm: %s api error: status %d: %s",
		provider, status, truncateRunes(body, maxErrorBodyRunes))
	switch status {
	case http.StatusUnauthorized: // 401
		msg += " (API キーを確認してください)"
	case http.StatusNotFound: // 404
		msg += " (モデル名を確認してください (OFFICE_LLM_MODEL))"
	}
	return errors.New(msg)
}

// truncateRunes はエラー本文を最大 n 文字（ルーン単位）に切り詰める。
// マルチバイト文字を壊さないように rune で扱う。
func truncateRunes(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…(truncated)"
}
