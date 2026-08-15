package config

import (
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	// Store (PostgreSQL only)
	StoreDSN      string `env:"STASH_POSTGRES_DSN,required"`
	VectorDim     int    `env:"STASH_VECTOR_DIM,required"`
	MaxResultSize int    `env:"STASH_MAX_RESULT_SIZE,required"`

	// OpenAI (embeddings + reasoning)
	OpenAIAPIKey                string        `env:"STASH_OPENAI_API_KEY,required"`
	OpenAIBaseURL               string        `env:"STASH_OPENAI_BASE_URL,required"`
	EmbeddingModel              string        `env:"STASH_EMBEDDING_MODEL,required"`
	EmbedderMaxRetries          int           `env:"STASH_EMBEDDER_MAX_RETRIES" envDefault:"1"`
	EmbedderRateLimitCooldown   time.Duration `env:"STASH_EMBEDDER_RATE_LIMIT_COOLDOWN" envDefault:"5m"`
	EmbedderPaymentCooldown     time.Duration `env:"STASH_EMBEDDER_PAYMENT_COOLDOWN" envDefault:"1h"`
	EmbedderServerErrorCooldown time.Duration `env:"STASH_EMBEDDER_SERVER_ERROR_COOLDOWN" envDefault:"1m"`
	ReasonerModel               string        `env:"STASH_REASONER_MODEL,required"`
	ReasonerAPIKey              string        `env:"STASH_REASONER_API_KEY"`
	ReasonerBaseURL             string        `env:"STASH_REASONER_BASE_URL"`
	ReasonerMaxTokens           int64         `env:"STASH_REASONER_MAX_TOKENS" envDefault:"4096"`
	ReasonerMaxRetries          int           `env:"STASH_REASONER_MAX_RETRIES" envDefault:"2"`
	ReasonerRateLimitCooldown   time.Duration `env:"STASH_REASONER_RATE_LIMIT_COOLDOWN" envDefault:"5m"`
	ReasonerPaymentCooldown     time.Duration `env:"STASH_REASONER_PAYMENT_COOLDOWN" envDefault:"1h"`
	ReasonerServerErrorCooldown time.Duration `env:"STASH_REASONER_SERVER_ERROR_COOLDOWN" envDefault:"1m"`

	// Memory
	ContextTTL time.Duration `env:"STASH_CONTEXT_TTL,required"`

	// Server
	HTTPAddr  string `env:"STASH_HTTP_ADDR,required"`
	LogLevel  string `env:"STASH_LOG_LEVEL,required"`
	LogFormat string `env:"STASH_LOG_FORMAT,required"`

	// Consolidation
	ConsolidationBatchSize           int     `env:"STASH_CONSOLIDATION_BATCH_SIZE" envDefault:"100"`
	ConsolidationSimilarityThreshold float64 `env:"STASH_CONSOLIDATION_SIMILARITY_THRESHOLD" envDefault:"0.85"`
	ConsolidationDedupThreshold      float64 `env:"STASH_CONSOLIDATION_DEDUP_THRESHOLD" envDefault:"0.85"`
	ConsolidationWindow              string  `env:"STASH_CONSOLIDATION_WINDOW" envDefault:"168h"`
	DecayFactor                      float64 `env:"STASH_DECAY_FACTOR" envDefault:"0.95"`
	ExpiryThreshold                  float32 `env:"STASH_EXPIRY_THRESHOLD" envDefault:"0.1"`
	HypothesisAutoConfirmThreshold   float32 `env:"STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD" envDefault:"0.9"`
	HypothesisAutoRejectThreshold    float32 `env:"STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD" envDefault:"0.9"`

	// Retrieval learning is deliberately independent from fact confidence.
	RetrievalLearningEnabled bool          `env:"STASH_RETRIEVAL_LEARNING_ENABLED" envDefault:"false"`
	RetrievalOverfetchFactor int           `env:"STASH_RETRIEVAL_OVERFETCH_FACTOR" envDefault:"3"`
	RetrievalUtilityWeight   float64       `env:"STASH_RETRIEVAL_UTILITY_WEIGHT" envDefault:"0.08"`
	RetrievalMaxUtilityDelta float64       `env:"STASH_RETRIEVAL_MAX_UTILITY_DELTA" envDefault:"0.10"`
	RecallHistoryRetention   time.Duration `env:"STASH_RECALL_HISTORY_RETENTION" envDefault:"2160h"`
	EmbeddingBackfillBatch   int           `env:"STASH_EMBEDDING_BACKFILL_BATCH" envDefault:"25"`
	// QueryInstruction enables asymmetric query encoding for the LIVE embedder.
	// Empty by default and must stay empty for symmetric models like bge-m3,
	// where wrapping a query corrupts its vector.
	QueryInstruction string `env:"STASH_QUERY_INSTRUCTION" envDefault:""`

	// Consolidation spend guardrails (TOL-295).
	//
	// MaxRecordAttempts bounds how often one record may fail a stage for a
	// permanent reason before the watermark moves past it. Unbounded, a single
	// unparseable record is re-sent to the paid reasoner every cycle forever.
	//
	// CycleReasonerCallCeiling is a hard per-namespace-per-cycle cap on reasoner
	// calls, enforced regardless of watermark state — the backstop for the
	// runaway we have not found yet.
	MaxRecordAttempts        int `env:"STASH_MAX_RECORD_ATTEMPTS" envDefault:"3"`
	CycleReasonerCallCeiling int `env:"STASH_CYCLE_REASONER_CALL_CEILING" envDefault:"50"`

	// Shadow embedding migration (TOL-297).
	//
	// Entirely optional and inert unless BOTH ShadowEmbeddingModel and
	// ShadowEmbeddingAPIKey are set. Nothing in the live read or write path
	// consults these; they exist so a second embedding representation can be
	// built alongside the live one and validated before any read-path swap.
	//
	// The API key deliberately reuses STASH_DEEPINFRA_EMBEDDING_API_KEY, the
	// name the scoped provider JWT is already stored under in production, so the
	// migration needs no credential move.
	// NOTE: every field here MUST carry an envDefault. This Config is parsed
	// with env.Options{RequiredIfNoDef: true}, so a tag without a default is
	// silently MANDATORY — which would force three new variables onto every
	// deployment and defeat the opt-in design entirely. Caught in production by
	// a failed healthcheck on 2026-08-14; see TestShadowConfigFieldsAreOptional.
	ShadowEmbeddingModel  string `env:"STASH_SHADOW_EMBEDDING_MODEL" envDefault:""`
	ShadowEmbeddingAPIKey string `env:"STASH_DEEPINFRA_EMBEDDING_API_KEY" envDefault:""`
	// Defaults to OpenAIBaseURL when empty.
	ShadowEmbeddingBaseURL string `env:"STASH_SHADOW_OPENAI_BASE_URL" envDefault:""`
	// Must stay under pgvector's 2000-dimension ceiling for an HNSW index on
	// the `vector` type. Qwen3-Embedding-8B honours this via Matryoshka
	// truncation: requesting 1024 returns exactly 1024 (measured 2026-08-14).
	ShadowVectorDim int `env:"STASH_SHADOW_VECTOR_DIM" envDefault:"1024"`
	// ShadowQueryInstruction turns on asymmetric query encoding for the shadow
	// model. Qwen3-Embedding expects it; leaving it empty costs 1-5% retrieval
	// performance per its model card.
	ShadowQueryInstruction string `env:"STASH_SHADOW_QUERY_INSTRUCTION" envDefault:"Given a question, retrieve the stored facts that answer it"`
	// Rows embedded per wave batch. Bounded so one command cannot spend without
	// limit; the wave driver reports what it did and exits.
	ShadowMigrateBatch int `env:"STASH_SHADOW_MIGRATE_BATCH" envDefault:"100"`

	// Provider reranking is opt-in. It reranks a bounded BGE-M3 candidate set
	// after semantic retrieval and falls back to that semantic ordering on any
	// provider failure. Qwen3-Reranker-0.6B is the verified DeepInfra contract.
	ProviderRerankModel          string `env:"STASH_DEEPINFRA_RERANK_MODEL" envDefault:""`
	ProviderRerankAPIKey         string `env:"STASH_DEEPINFRA_RERANK_API_KEY" envDefault:""`
	ProviderRerankBaseURL        string `env:"STASH_DEEPINFRA_RERANK_BASE_URL" envDefault:"https://api.deepinfra.com"`
	ProviderRerankCandidateLimit int    `env:"STASH_DEEPINFRA_RERANK_CANDIDATE_LIMIT" envDefault:"50"`
}

// ShadowEnabled reports whether a shadow embedding representation is configured.
// Both the model and its credential are required: a model without a key would
// fail every call, and a key without a model has nothing to embed with.
func (c *Config) ShadowEnabled() bool {
	return c.ShadowEmbeddingModel != "" && c.ShadowEmbeddingAPIKey != ""
}

// ShadowBaseURL resolves the shadow provider endpoint, defaulting to the live one.
func (c *Config) ShadowBaseURL() string {
	if c.ShadowEmbeddingBaseURL != "" {
		return c.ShadowEmbeddingBaseURL
	}
	return c.OpenAIBaseURL
}

func (c *Config) ProviderRerankEnabled() bool {
	return c.ProviderRerankModel != "" && c.ProviderRerankAPIKey != ""
}

func NewFromFile(filename string) (*Config, error) {
	if _, err := os.Stat(filename); err == nil {
		if err := godotenv.Load(filename); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	cfg := &Config{}
	opts := env.Options{
		RequiredIfNoDef: true,
	}
	if err := env.ParseWithOptions(cfg, opts); err != nil {
		return nil, err
	}
	return cfg, nil
}
