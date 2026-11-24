package brain

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeepInfraRerankerUsesParallelQueriesAndPreservesOrdering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/inference/Qwen/Qwen3-Reranker-0.6B" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		want := `{"queries":["q","q"],"documents":["a","b"]}`
		if string(body) != want {
			t.Fatalf("body = %s, want %s", body, want)
		}
		_, _ = w.Write([]byte(`{"scores":[0.1,0.9]}`))
	}))
	defer server.Close()

	reranker, err := NewDeepInfraReranker(server.URL, "test-key", "Qwen/Qwen3-Reranker-0.6B")
	if err != nil {
		t.Fatal(err)
	}
	scores, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0] != 0.1 || scores[1] != 0.9 {
		t.Fatalf("scores = %#v", scores)
	}
}

func TestDeepInfraRerankerRejectsMismatchedScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"scores":[0.1]}`))
	}))
	defer server.Close()
	reranker, err := NewDeepInfraReranker(server.URL, "test-key", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reranker.Rerank(context.Background(), "q", []string{"a", "b"}); err == nil {
		t.Fatal("expected mismatched score error")
	}
}
