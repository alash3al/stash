package embedder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderCircuitStopsRepeatedEmbeddingRequests(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted","type":"provider_error"}}`))
	}))
	defer server.Close()

	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	emb, err := NewOpenAIWithConfig(server.URL, "test-key", "test-model", 3, OpenAIConfig{
		MaxRetries:        0,
		RateLimitCooldown: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	emb.now = func() time.Time { return now }

	if _, err := emb.Embed(context.Background(), "first"); !IsUnavailable(err) {
		t.Fatalf("first error = %v, want UnavailableError", err)
	}
	if _, err := emb.Embed(context.Background(), "second"); !IsUnavailable(err) {
		t.Fatalf("open-circuit error = %v, want UnavailableError", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("open circuit made %d requests, want 1", got)
	}
}

func TestEmbeddingResponseDimensionIsValidated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"test","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	}))
	defer server.Close()

	emb, err := NewOpenAIWithConfig(server.URL, "test-key", "test-model", 3, OpenAIConfig{MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emb.Embed(context.Background(), "dimension mismatch"); err == nil {
		t.Fatal("dimension mismatch was accepted")
	}
}
