package gh

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerReturnsNotImplemented(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", nil)

	Handler()(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Error("Content-Type が設定されていない")
	}
}
