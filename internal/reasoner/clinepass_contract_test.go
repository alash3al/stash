package reasoner

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestClinePassContract is an opt-in live canary for the production reasoner
// contract. CI skips it unless a short-lived credential is injected.
func TestClinePassContract(t *testing.T) {
	key := os.Getenv("STASH_TEST_CLINEPASS_API_KEY")
	if key == "" {
		t.Skip("STASH_TEST_CLINEPASS_API_KEY is not set")
	}
	r, err := NewOpenAIWithConfig("https://api.cline.bot/api/v1", key, "cline-pass/glm-5.2", OpenAIConfig{
		MaxTokens:           4096,
		MaxRetries:          0,
		RateLimitCooldown:   time.Minute,
		PaymentCooldown:     time.Hour,
		ServerErrorCooldown: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := r.ReasonStructured(context.Background(), []string{"Stash runs on Railway and stores durable memory in PostgreSQL."})
	if err != nil {
		t.Fatal(err)
	}
	if fact == nil || fact.Summary == "" {
		t.Fatalf("ClinePass returned an empty structured fact: %+v", fact)
	}
}
