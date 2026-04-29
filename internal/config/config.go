package config

import (
	"errors"
	"fmt"
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
	OpenAIAPIKey   string `env:"STASH_OPENAI_API_KEY,required"`
	OpenAIBaseURL  string `env:"STASH_OPENAI_BASE_URL,required"`
	EmbeddingModel string `env:"STASH_EMBEDDING_MODEL,required"`
	ReasonerModel  string `env:"STASH_REASONER_MODEL,required"`

	// Memory
	ContextTTL time.Duration `env:"STASH_CONTEXT_TTL,required"`

	// Server
	HTTPAddr  string `env:"STASH_HTTP_ADDR,required"`
	LogLevel  string `env:"STASH_LOG_LEVEL,required"`
	LogFormat string `env:"STASH_LOG_FORMAT,required"`

	// Database pool sizing
	DBMaxConns int32 `env:"STASH_DB_MAX_CONNS" envDefault:"25"`
	DBMinConns int32 `env:"STASH_DB_MIN_CONNS" envDefault:"5"`

	// Consolidation
	ConsolidationBatchSize           int           `env:"STASH_CONSOLIDATION_BATCH_SIZE" envDefault:"100"`
	ConsolidationSimilarityThreshold float64       `env:"STASH_CONSOLIDATION_SIMILARITY_THRESHOLD" envDefault:"0.85"`
	ConsolidationDedupThreshold      float64       `env:"STASH_CONSOLIDATION_DEDUP_THRESHOLD" envDefault:"0.95"`
	ConsolidationWindow              time.Duration `env:"STASH_CONSOLIDATION_WINDOW" envDefault:"168h"`
	DecayFactor                      float64       `env:"STASH_DECAY_FACTOR" envDefault:"0.95"`
	ExpiryThreshold                  float32       `env:"STASH_EXPIRY_THRESHOLD" envDefault:"0.1"`
	HypothesisAutoConfirmThreshold   float32       `env:"STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD" envDefault:"0.9"`
	HypothesisAutoRejectThreshold    float32       `env:"STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD" envDefault:"0.9"`
}

// Validate checks cross-field invariants that cannot be expressed via struct
// tags alone. All violations are accumulated and returned together so that
// operators see the full misconfiguration picture on a single startup failure.
func (c *Config) Validate() error {
	var errs []error

	if c.VectorDim <= 0 {
		errs = append(errs, fmt.Errorf("STASH_VECTOR_DIM must be > 0, got %d", c.VectorDim))
	}

	if c.DBMinConns >= c.DBMaxConns {
		errs = append(errs, fmt.Errorf(
			"STASH_DB_MIN_CONNS (%d) must be < STASH_DB_MAX_CONNS (%d)",
			c.DBMinConns, c.DBMaxConns,
		))
	}

	if c.ConsolidationWindow <= 0 {
		errs = append(errs, fmt.Errorf(
			"STASH_CONSOLIDATION_WINDOW must be a positive duration, got %s",
			c.ConsolidationWindow,
		))
	}

	if c.ConsolidationSimilarityThreshold >= c.ConsolidationDedupThreshold {
		errs = append(errs, fmt.Errorf(
			"STASH_CONSOLIDATION_SIMILARITY_THRESHOLD (%.4f) must be < STASH_CONSOLIDATION_DEDUP_THRESHOLD (%.4f): "+
				"dedup fires after similarity grouping, so dedup threshold must be stricter",
			c.ConsolidationSimilarityThreshold, c.ConsolidationDedupThreshold,
		))
	}

	if c.HypothesisAutoConfirmThreshold <= c.HypothesisAutoRejectThreshold {
		errs = append(errs, fmt.Errorf(
			"STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD (%.4f) must be > STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD (%.4f): "+
				"equal thresholds create an ambiguous branch where a score qualifies for both outcomes simultaneously",
			c.HypothesisAutoConfirmThreshold, c.HypothesisAutoRejectThreshold,
		))
	}

	return errors.Join(errs...)
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
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}
