package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/alash3al/stash/internal/brain"
	"github.com/alash3al/stash/internal/config"
	"github.com/alash3al/stash/internal/db"
	"github.com/alash3al/stash/internal/embedder"
	"github.com/alash3al/stash/internal/queries"
	"github.com/alash3al/stash/internal/reasoner"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Context holds all initialized services.
type Context struct {
	Config *config.Config
	Brain  *brain.Brain
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// Embedder is the LIVE embedder, exposed so the shadow migration can
	// compare both representations against the same query.
	Embedder embedder.Embedder

	// Shadow is nil unless a shadow embedding model and credential are both
	// configured. Nil is the normal production state: the shadow migration is
	// opt-in and nothing in the live path depends on it.
	Shadow *brain.ShadowMigrator
}

// MustNew panics on bootstrap failure.
func MustNew(ctx context.Context) *Context {
	bc, err := New(ctx)
	if err != nil {
		panic(fmt.Sprintf("bootstrap failed: %v", err))
	}
	return bc
}

// New initializes all services: database, embedder, reasoner, queries, brain.
func New(ctx context.Context) (*Context, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	logger := buildLogger(cfg)

	pool, err := db.Open(ctx, cfg.StoreDSN, cfg.EmbeddingModel, cfg.VectorDim)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	emb, err := buildEmbedder(cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("build embedder: %w", err)
	}

	// Wrap embedder with pgx-backed cache
	cachedEmb := embedder.NewCached(emb, pool)

	reas, err := buildReasoner(cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("build reasoner: %w", err)
	}

	q, err := queries.New()
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("load queries: %w", err)
	}

	window, err := time.ParseDuration(cfg.ConsolidationWindow)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("parse consolidation window: %w", err)
	}

	brainConfig := brain.Config{
		BatchSize:                      cfg.ConsolidationBatchSize,
		SimilarityThreshold:            cfg.ConsolidationSimilarityThreshold,
		DedupThreshold:                 cfg.ConsolidationDedupThreshold,
		Window:                         window,
		DecayFactor:                    cfg.DecayFactor,
		ExpiryThreshold:                cfg.ExpiryThreshold,
		HypothesisAutoConfirmThreshold: cfg.HypothesisAutoConfirmThreshold,
		HypothesisAutoRejectThreshold:  cfg.HypothesisAutoRejectThreshold,
		RetrievalLearningEnabled:       cfg.RetrievalLearningEnabled,
		RetrievalOverfetchFactor:       cfg.RetrievalOverfetchFactor,
		RetrievalUtilityWeight:         cfg.RetrievalUtilityWeight,
		RetrievalMaxUtilityDelta:       cfg.RetrievalMaxUtilityDelta,
		RecallHistoryRetention:         cfg.RecallHistoryRetention,
		EmbeddingBackfillBatch:         cfg.EmbeddingBackfillBatch,
		ProviderRerankCandidateLimit:   cfg.ProviderRerankCandidateLimit,
		MaxRecordAttempts:              cfg.MaxRecordAttempts,
		CycleReasonerCallCeiling:       cfg.CycleReasonerCallCeiling,
	}
	if cfg.ProviderRerankEnabled() {
		reranker, err := brain.NewDeepInfraReranker(cfg.ProviderRerankBaseURL, cfg.ProviderRerankAPIKey, cfg.ProviderRerankModel)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("build provider reranker: %w", err)
		}
		brainConfig.ProviderReranker = reranker
		logger.Info("DeepInfra provider reranking configured", "model", cfg.ProviderRerankModel, "candidate_limit", cfg.ProviderRerankCandidateLimit)
	}
	br, err := brain.New(pool, cachedEmb, reas, q, brainConfig)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("build brain: %w", err)
	}

	// Shadow migrator: opt-in, and a hard failure if it is configured but
	// cannot be built. A half-configured migration must not look like a
	// disabled one.
	var shadow *brain.ShadowMigrator
	if cfg.ShadowEnabled() {
		shadowEmb, err := buildShadowEmbedder(cfg)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("build shadow embedder: %w", err)
		}
		shadow, err = brain.NewShadowMigrator(pool, shadowEmb, cfg.ShadowEmbeddingModel, cfg.ShadowVectorDim, cfg.ShadowMigrateBatch)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("build shadow migrator: %w", err)
		}
		logger.Info("shadow embedding configured",
			"model", cfg.ShadowEmbeddingModel, "dims", cfg.ShadowVectorDim, "batch", cfg.ShadowMigrateBatch)
	}

	return &Context{
		Config:   cfg,
		Brain:    br,
		Pool:     pool,
		Logger:   logger,
		Embedder: cachedEmb,
		Shadow:   shadow,
	}, nil
}

// buildShadowEmbedder constructs the second embedder used only by the shadow
// migration. It is intentionally NOT wrapped in the pgx embedding cache: every
// row is embedded exactly once during a migration, so caching would add ~15k
// rows of write traffic to embedding_cache for no reuse.
func buildShadowEmbedder(cfg *config.Config) (embedder.Embedder, error) {
	return embedder.NewOpenAIWithConfig(
		cfg.ShadowBaseURL(),
		cfg.ShadowEmbeddingAPIKey,
		cfg.ShadowEmbeddingModel,
		cfg.ShadowVectorDim,
		embedder.OpenAIConfig{
			MaxRetries:          cfg.EmbedderMaxRetries,
			RateLimitCooldown:   cfg.EmbedderRateLimitCooldown,
			PaymentCooldown:     cfg.EmbedderPaymentCooldown,
			ServerErrorCooldown: cfg.EmbedderServerErrorCooldown,
			QueryInstruction:    cfg.ShadowQueryInstruction,
		},
	)
}

// Close releases all resources.
func (c *Context) Close() error {
	var errs []string
	if c.Brain != nil {
		c.Brain.Close()
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func loadConfig() (*config.Config, error) {
	filename := os.Getenv("STASHCONFIG")
	if filename == "" {
		filename = ".env"
	}
	return config.NewFromFile(filename)
}

func buildLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{}

	switch cfg.LogLevel {
	case "debug":
		opts.Level = slog.LevelDebug
	case "info":
		opts.Level = slog.LevelInfo
	case "warn":
		opts.Level = slog.LevelWarn
	case "error":
		opts.Level = slog.LevelError
	default:
		opts.Level = slog.LevelInfo
	}

	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func buildEmbedder(cfg *config.Config) (embedder.Embedder, error) {
	return embedder.NewOpenAIWithConfig(
		cfg.OpenAIBaseURL,
		cfg.OpenAIAPIKey,
		cfg.EmbeddingModel,
		cfg.VectorDim,
		embedder.OpenAIConfig{
			MaxRetries:          cfg.EmbedderMaxRetries,
			RateLimitCooldown:   cfg.EmbedderRateLimitCooldown,
			PaymentCooldown:     cfg.EmbedderPaymentCooldown,
			ServerErrorCooldown: cfg.EmbedderServerErrorCooldown,
			QueryInstruction:    cfg.QueryInstruction,
		},
	)
}

func buildReasoner(cfg *config.Config) (reasoner.Reasoner, error) {
	baseURL := cfg.ReasonerBaseURL
	if baseURL == "" {
		baseURL = cfg.OpenAIBaseURL
	}
	apiKey := cfg.ReasonerAPIKey
	if apiKey == "" {
		apiKey = cfg.OpenAIAPIKey
	}
	return reasoner.NewOpenAIWithConfig(
		baseURL,
		apiKey,
		cfg.ReasonerModel,
		reasoner.OpenAIConfig{
			MaxTokens:           cfg.ReasonerMaxTokens,
			MaxRetries:          cfg.ReasonerMaxRetries,
			RateLimitCooldown:   cfg.ReasonerRateLimitCooldown,
			PaymentCooldown:     cfg.ReasonerPaymentCooldown,
			ServerErrorCooldown: cfg.ReasonerServerErrorCooldown,
		},
	)
}
