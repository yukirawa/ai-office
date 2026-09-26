package llm

import (
	"context"
	"encoding/json"
	"sync"
)

// defaultMockResponse は MockClient の応答をすべて使い切った後に返す安定した文言。
// 設計書の例に合わせている（エージェントのテストで予測可能にするため）。
const defaultMockResponse = "了解しました。本日のタスクを整理します。"

// mockModel は MockClient が返す Model 名。実 API と区別できるようにする。
const mockModel = "mock"

// MockClient はネットワークアクセスを行わない Client 実装。
// 事前に登録した応答を順番に返し、受け取った Request を記録する（テストの検証用）。
type MockClient struct {
	mu        sync.Mutex
	responses []string
	idx       int
	calls     []Request
}

// NewMock は responses を順番に返す MockClient を生成する。
// 応答を使い切った後は defaultMockResponse を返し続ける。
func NewMock(responses ...string) *MockClient {
	return &MockClient{
		responses: append([]string(nil), responses...),
	}
}

// Chat は次の応答を返す。ctx がキャンセル済みならその error を返す。
func (m *MockClient) Chat(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	m.mu.Lock()
	m.calls = append(m.calls, cloneRequest(req))
	text := defaultMockResponse
	if m.idx < len(m.responses) {
		text = m.responses[m.idx]
		m.idx++
	}
	m.mu.Unlock()

	return Response{
		Text:  text,
		Model: mockModel,
		// プロバイダ非依存のモックなので、OpenAI/DeepSeek の終端理由（"stop"）を返す。
		// （Anthropic の "end_turn" でも終端だが、stop を既定にすることで
		//   終端判定のプロバイダ差異をテストで踏みやすくする）
		StopReason: "stop",
	}, nil
}

// Requests は記録した Request のコピーを返す（呼び出し側の変更が内部状態に影響しないように）。
func (m *MockClient) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.calls))
	for i, r := range m.calls {
		out[i] = cloneRequest(r)
	}
	return out
}

// cloneRequest は Request を浅くコピーし、スライスだけ深くコピーする。
func cloneRequest(r Request) Request {
	c := r
	if r.Messages != nil {
		c.Messages = append([]Message(nil), r.Messages...)
	}
	if r.Tools != nil {
		c.Tools = make([]Tool, len(r.Tools))
		for i, t := range r.Tools {
			c.Tools[i] = t
			if t.Schema != nil {
				c.Tools[i].Schema = append(json.RawMessage(nil), t.Schema...)
			}
		}
	}
	return c
}
