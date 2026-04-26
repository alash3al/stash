package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

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

	pool, err := db.Open(ctx, cfg.StoreDSN, cfg.EmbeddingModel, cfg.VectorDim, db.PoolConfig{
		MaxConns: cfg.DBMaxConns,
		MinConns: cfg.DBMinConns,
	})
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

	br, err := brain.New(pool, cachedEmb, reas, q, brain.Config{
		BatchSize:                      cfg.ConsolidationBatchSize,
		SimilarityThreshold:            cfg.ConsolidationSimilarityThreshold,
		DedupThreshold:                 cfg.ConsolidationDedupThreshold,
		Window:                         cfg.ConsolidationWindow,
		DecayFactor:                    cfg.DecayFactor,
		ExpiryThreshold:                cfg.ExpiryThreshold,
		HypothesisAutoConfirmThreshold: cfg.HypothesisAutoConfirmThreshold,
		HypothesisAutoRejectThreshold:  cfg.HypothesisAutoRejectThreshold,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("build brain: %w", err)
	}

	return &Context{
		Config: cfg,
		Brain:  br,
		Pool:   pool,
		Logger: logger,
	}, nil
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
	return embedder.NewOpenAI(
		cfg.OpenAIBaseURL,
		cfg.OpenAIAPIKey,
		cfg.EmbeddingModel,
		cfg.VectorDim,
	)
}

func buildReasoner(cfg *config.Config) (reasoner.Reasoner, error) {
	return reasoner.NewOpenAI(
		cfg.OpenAIBaseURL,
		cfg.OpenAIAPIKey,
		cfg.ReasonerModel,
	)
}
