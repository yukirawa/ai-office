package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModels(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"},{"id":""}]}`))
	}))
	defer srv.Close()

	c := NewAnthropic("secret-key", srv.URL)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotKey != "secret-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "m2" {
		t.Errorf("models = %v, want [m1 m2]", models)
	}
}

func TestModelsErrorHasHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewAnthropic("bad", srv.URL, WithMaxRetries(0))
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "API キーを確認してください") {
		t.Errorf("ヒントが含まれていません: %v", err)
	}
}
