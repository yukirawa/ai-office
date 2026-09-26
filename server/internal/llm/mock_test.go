package llm

import (
	"context"
	"testing"
)

func TestMockReturnsResponsesInOrder(t *testing.T) {
	client := NewMock("一つ目", "二つ目")
	ctx := context.Background()

	want := []string{"一つ目", "二つ目", defaultMockResponse, defaultMockResponse}
	for i, w := range want {
		resp, err := client.Chat(ctx, Request{Model: "m"})
		if err != nil {
			t.Fatalf("call %d: 予期しないエラー: %v", i, err)
		}
		if resp.Text != w {
			t.Errorf("call %d: Text = %q, want %q", i, resp.Text, w)
		}
		if resp.Model != mockModel {
			t.Errorf("call %d: Model = %q, want %q", i, resp.Model, mockModel)
		}
	}
}

func TestMockRecordsRequests(t *testing.T) {
	client := NewMock()
	ctx := context.Background()

	req1 := Request{Model: "a", System: "sys", Messages: []Message{{Role: "user", Content: "1"}}}
	req2 := Request{Model: "b", Messages: []Message{{Role: "user", Content: "2"}}}
	if _, err := client.Chat(ctx, req1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(ctx, req2); err != nil {
		t.Fatal(err)
	}

	got := client.Requests()
	if len(got) != 2 {
		t.Fatalf("Requests() len = %d, want 2", len(got))
	}
	if got[0].Model != "a" || got[1].Model != "b" {
		t.Errorf("記録順が異なります: %+v", got)
	}
	if got[0].Messages[0].Content != "1" {
		t.Errorf("メッセージが記録されていません: %+v", got[0])
	}
}

func TestMockRequestsReturnsCopy(t *testing.T) {
	client := NewMock()
	ctx := context.Background()
	if _, err := client.Chat(ctx, Request{Model: "a"}); err != nil {
		t.Fatal(err)
	}

	first := client.Requests()
	first[0].Model = "改ざん"

	second := client.Requests()
	if second[0].Model != "a" {
		t.Errorf("Requests() は内部状態のコピーを返すべきです: got %q", second[0].Model)
	}
}

func TestMockRespectsCanceledContext(t *testing.T) {
	client := NewMock("x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Chat(ctx, Request{}); err == nil {
		t.Fatal("キャンセル済み ctx ではエラーを返すべきです")
	}
}
