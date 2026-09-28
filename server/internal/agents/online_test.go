package agents

// online_test.go は Manager のオンライン優先割当（SetOnlineFunc / nextAssignee）を検証する。

import (
	"testing"

	"github.com/yukirawa/ai-office/server/internal/persona"
)

// TestNextAssigneePrefersOnline は SetOnlineFunc により、計画文がオフラインの dev を
// 名指ししてもオンラインの dev が選ばれること、nil に戻すと制限が外れることを確認する。
func TestNextAssigneePrefersOnline(t *testing.T) {
	devM := NewDevAgent("dev_m", persona.Persona{Name: "タクミ"}, nil, nil, nil, nil, nil, nil, Options{})
	devF := NewDevAgent("dev_f", persona.Persona{Name: "ミナ"}, nil, nil, nil, nil, nil, nil, Options{})
	mgr := NewManager("mgr", persona.Persona{Name: "ミカ"}, nil, nil, Options{})
	mgr.SetAssignees(devM, devF)

	// dev_f だけがオンライン。
	mgr.SetOnlineFunc(func(id string) bool { return id == "dev_f" })

	// 計画がオフラインの dev_m を名指ししても、オンラインの dev_f が選ばれる。
	if got := mgr.nextAssignee("担当: dev_m で"); got == nil || got.ID() != "dev_f" {
		t.Fatalf("オンライン優先が効いていません: got=%v, want dev_f", got)
	}

	// オンラインの dev_f を名指しした場合は dev_f。
	if got := mgr.nextAssignee("担当: dev_f で"); got == nil || got.ID() != "dev_f" {
		t.Fatalf("dev_f 名指し: got=%v, want dev_f", got)
	}

	// SetOnlineFunc(nil) に戻すとオンライン制限が外れ、名指しがそのまま優先される。
	mgr.SetOnlineFunc(nil)
	if got := mgr.nextAssignee("担当: dev_m で"); got == nil || got.ID() != "dev_m" {
		t.Fatalf("オンライン制限解除後: got=%v, want dev_m", got)
	}
}
