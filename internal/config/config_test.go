package config

import (
	"reflect"
	"strings"
	"testing"
)

// This Config is parsed with env.Options{RequiredIfNoDef: true}, which means a
// struct tag WITHOUT an `envDefault` is silently mandatory. That is a trap: a
// field meant to be optional boots fine on any machine where it happens to be
// set, and hard-fails everywhere else.
//
// It bit us for real on 2026-08-14 — the shadow embedding fields shipped
// without defaults, which turned an explicitly opt-in feature into three newly
// required variables. Production caught it as a failed healthcheck
// ("required environment variable STASH_SHADOW_OPENAI_BASE_URL is not set")
// and rolled back, but the deploy was dead on arrival.
func TestShadowConfigFieldsAreOptional(t *testing.T) {
	optional := []string{
		"ShadowEmbeddingModel",
		"ShadowEmbeddingAPIKey",
		"ShadowEmbeddingBaseURL",
		"ShadowVectorDim",
		"ShadowMigrateBatch",
	}

	typ := reflect.TypeOf(Config{})
	for _, name := range optional {
		field, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("Config has no field %s", name)
		}
		tag := string(field.Tag)
		if !strings.Contains(tag, "envDefault:") {
			t.Errorf("%s has no envDefault; with RequiredIfNoDef:true that makes it MANDATORY "+
				"and every deployment without it will fail to boot. Tag: %s", name, tag)
		}
		if strings.Contains(tag, ",required") {
			t.Errorf("%s is marked required but shadow embedding is opt-in. Tag: %s", name, tag)
		}
	}
}

func TestShadowEnabledNeedsBothModelAndCredential(t *testing.T) {
	// A model without a key would 403 on every call; a key without a model has
	// nothing to embed with. Either alone must read as "not configured" rather
	// than half-arming the migration.
	cases := []struct {
		name  string
		model string
		key   string
		want  bool
	}{
		{"neither", "", "", false},
		{"model only", "Qwen/Qwen3-Embedding-8B", "", false},
		{"credential only", "", "sk-test", false},
		{"both", "Qwen/Qwen3-Embedding-8B", "sk-test", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{ShadowEmbeddingModel: c.model, ShadowEmbeddingAPIKey: c.key}
			if got := cfg.ShadowEnabled(); got != c.want {
				t.Fatalf("ShadowEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestProviderRerankConfigIsOptionalAndNeedsModelAndCredential(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for _, name := range []string{"ProviderRerankModel", "ProviderRerankAPIKey", "ProviderRerankBaseURL", "ProviderRerankCandidateLimit"} {
		field, ok := typ.FieldByName(name)
		if !ok || !strings.Contains(string(field.Tag), "envDefault:") {
			t.Fatalf("%s must remain optional, field=%+v", name, field)
		}
	}
	if (&Config{ProviderRerankModel: "Qwen/Qwen3-Reranker-0.6B"}).ProviderRerankEnabled() {
		t.Fatal("model without credential must not enable provider reranking")
	}
	if (&Config{ProviderRerankAPIKey: "test"}).ProviderRerankEnabled() {
		t.Fatal("credential without model must not enable provider reranking")
	}
	if !(&Config{ProviderRerankModel: "Qwen/Qwen3-Reranker-0.6B", ProviderRerankAPIKey: "test"}).ProviderRerankEnabled() {
		t.Fatal("model and credential must enable provider reranking")
	}
}

func TestShadowBaseURLFallsBackToTheLiveEndpoint(t *testing.T) {
	cfg := &Config{OpenAIBaseURL: "https://api.deepinfra.com/v1/openai"}
	if got := cfg.ShadowBaseURL(); got != "https://api.deepinfra.com/v1/openai" {
		t.Fatalf("unset shadow base URL should fall back to the live one, got %q", got)
	}

	cfg.ShadowEmbeddingBaseURL = "https://other.example/v1"
	if got := cfg.ShadowBaseURL(); got != "https://other.example/v1" {
		t.Fatalf("explicit shadow base URL must win, got %q", got)
	}
}
