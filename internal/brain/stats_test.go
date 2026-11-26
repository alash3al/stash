package brain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewProviderInfo(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		model      string
		wantHost   string
		wantName   string
		wantCFFlag bool
	}{
		{"deepinfra", "https://api.deepinfra.com/v1/openai", "BAAI/bge-m3", "api.deepinfra.com", "deepinfra", false},
		{"openrouter", "https://openrouter.ai/api/v1", "m", "openrouter.ai", "openrouter", false},
		{"cloudflare ai gateway", "https://gateway.ai.cloudflare.com/v1/acct/gw/openai", "m", "gateway.ai.cloudflare.com", "cloudflare", true},
		{"cloudflare workers", "https://proxy.example.workers.dev/v1", "m", "proxy.example.workers.dev", "cloudflare", true},
		{"cloudflare lookalike is not cloudflare", "https://notcloudflare.com/v1", "m", "notcloudflare.com", "notcloudflare.com", false},
		{"localhost with port", "http://localhost:11434/v1", "nomic-embed-text", "localhost", "local", false},
		{"unknown host", "https://llm.internal.example/v1", "m", "llm.internal.example", "llm.internal.example", false},
		{"uppercase host", "https://API.DeepInfra.COM/v1", "m", "api.deepinfra.com", "deepinfra", false},
		{"empty", "", "m", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewProviderInfo(tt.baseURL, tt.model)
			if got.BaseURLHost != tt.wantHost || got.Provider != tt.wantName || got.CloudflareHost != tt.wantCFFlag || got.Model != tt.model {
				t.Fatalf("NewProviderInfo(%q) = %+v; want host=%q provider=%q cloudflare=%v",
					tt.baseURL, got, tt.wantHost, tt.wantName, tt.wantCFFlag)
			}
		})
	}
}

// The base URL can carry userinfo or a key in its query string. None of it may
// reach the serialized report.
func TestNewProviderInfoNeverLeaksCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://user:s3cret-pass@api.deepinfra.com/v1?api_key=s3cret-key",
		"https://api.deepinfra.com/v1/s3cret-path?key=s3cret-key#s3cret-frag",
	} {
		b, err := json.Marshal(NewProviderInfo(raw, "model-x"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "s3cret") {
			t.Fatalf("credential material leaked for %q: %s", raw, b)
		}
	}
	// An unparseable URL must not be echoed back either.
	b, _ := json.Marshal(NewProviderInfo("http://s3cret\x7f@bad host/", "m"))
	if strings.Contains(string(b), "s3cret") {
		t.Fatalf("unparseable URL echoed: %s", b)
	}
}
