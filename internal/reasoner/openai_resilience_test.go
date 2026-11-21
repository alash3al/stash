package reasoner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go"
)

func TestProviderCircuitOpensForQuotaErrorsAndRecovers(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusPaymentRequired, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int64
			currentStatus := atomic.Int64{}
			currentStatus.Store(int64(status))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if int(currentStatus.Load()) != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(int(currentStatus.Load()))
					_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable","type":"provider_error"}}`))
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if got := int64(body["max_tokens"].(float64)); got != 777 {
					t.Errorf("max_tokens = %d, want 777", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
			reasoner, err := NewOpenAIWithConfig(server.URL, "test-key", "test-model", OpenAIConfig{
				MaxTokens:         777,
				MaxRetries:        0,
				RateLimitCooldown: time.Minute,
				PaymentCooldown:   time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			reasoner.now = func() time.Time { return now }
			messages := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("test")}

			if _, err := reasoner.completion(context.Background(), messages); !IsUnavailable(err) {
				t.Fatalf("first error = %v, want UnavailableError", err)
			}
			if _, err := reasoner.completion(context.Background(), messages); !IsUnavailable(err) {
				t.Fatalf("circuit error = %v, want UnavailableError", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("open circuit made %d provider requests, want 1", got)
			}

			currentStatus.Store(http.StatusOK)
			if status == http.StatusTooManyRequests {
				now = now.Add(time.Minute + time.Second)
			} else {
				now = now.Add(time.Hour + time.Second)
			}
			if _, err := reasoner.completion(context.Background(), messages); err != nil {
				t.Fatalf("completion after cooldown: %v", err)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("requests after recovery = %d, want 2", got)
			}
		})
	}
}
