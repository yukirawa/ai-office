package llm

import "time"

// 既定値。テストからも参照されるためパッケージ定数として定義している。
const (
	// defaultHTTPTimeout は 1 リクエストの上限時間。
	defaultHTTPTimeout = 120 * time.Second
	// defaultMaxRetries は初回に加えて行うリトライ回数の既定値。
	defaultMaxRetries = 3
	// defaultRetryBaseDelay は指数バックオフの基準値。
	defaultRetryBaseDelay = 500 * time.Millisecond
)

// clientOptions は各 LLM クライアント共通の設定。
// Anthropic / DeepSeek などプロバイダに依存しない。
type clientOptions struct {
	timeout        time.Duration // default 120s
	maxRetries     int           // extra retries (1+n attempts); default 3; <0 → 0
	retryBaseDelay time.Duration // default 500ms
}

// defaultClientOptions は既定のクライアント設定を返す。
func defaultClientOptions() clientOptions {
	return clientOptions{
		timeout:        defaultHTTPTimeout,
		maxRetries:     defaultMaxRetries,
		retryBaseDelay: defaultRetryBaseDelay,
	}
}

// newClientOptions は既定値に opts を順番に適用し、正規化した設定を返す。
// timeout は WithTimeout で明示された値をそのまま尊重する（0 以下でも上書きしない）。
func newClientOptions(opts ...Option) clientOptions {
	o := defaultClientOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.maxRetries < 0 {
		o.maxRetries = 0
	}
	if o.retryBaseDelay <= 0 {
		o.retryBaseDelay = defaultRetryBaseDelay
	}
	return o
}

// Option は LLM クライアントを生成時に構成するための関数。
// プロバイダ非依存にして Anthropic / DeepSeek で共有する。
type Option func(*clientOptions)

// WithTimeout は HTTP クライアントのタイムアウトを設定する。既定は 120s。
func WithTimeout(d time.Duration) Option {
	return func(o *clientOptions) {
		o.timeout = d
	}
}

// WithMaxRetries は初回に加えて行うリトライ回数を設定する（初回 + n）。
// 既定は 3。0 を指定するとリトライしない。負の値は 0 として扱う。
func WithMaxRetries(n int) Option {
	return func(o *clientOptions) {
		if n < 0 {
			n = 0
		}
		o.maxRetries = n
	}
}

// WithRetryBaseDelay は指数バックオフの基準値を設定する。既定は 500ms。
// 0 以下を指定した場合は既定値のままにする。
func WithRetryBaseDelay(d time.Duration) Option {
	return func(o *clientOptions) {
		if d > 0 {
			o.retryBaseDelay = d
		}
	}
}
