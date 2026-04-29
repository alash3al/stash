package db

import (
	"context"
	"embed"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}
func (discardLogger) Fatalf(string, ...any) {}

//go:embed migrations/*.sql
var embedMigrations embed.FS

// PoolConfig holds connection pool sizing parameters.
type PoolConfig struct {
	MaxConns int32
	MinConns int32
}

// DefaultPoolConfig returns conservative defaults suitable for local development.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{MaxConns: 25, MinConns: 5}
}

// Open creates a pgxpool, runs goose migrations, and validates the embedding model setting.
// Returns the pool for application use. The caller is responsible for calling pool.Close().
func Open(ctx context.Context, dsn string, expectedModel string, vectorDim int, poolCfg PoolConfig) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}

	config.MaxConns = poolCfg.MaxConns
	config.MinConns = poolCfg.MinConns
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	config.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgxpool.Ping: %w", err)
	}

	// Open a *sql.DB backed by pgx for goose migrations.
	connConfig := pool.Config().ConnConfig
	sqlDB := stdlib.OpenDB(*connConfig)
	defer sqlDB.Close()

	goose.SetBaseFS(embedMigrations)
	goose.SetLogger(discardLogger{})

	if err := goose.SetDialect("postgres"); err != nil {
		return nil, fmt.Errorf("goose.SetDialect: %w", err)
	}

	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return nil, fmt.Errorf("goose.Up: %w", err)
	}

	// Set vector dimensions on columns that use unconstrained `vector` type.
	vectorColumns := []struct {
		table  string
		column string
	}{
		{"episodes", "embedding"},
		{"facts", "embedding"},
		{"embedding_cache", "embedding"},
	}
	for _, vc := range vectorColumns {
		var currentDim int
		if err := pool.QueryRow(ctx,
			"SELECT atttypmod FROM pg_attribute a JOIN pg_class c ON a.attrelid = c.oid WHERE c.relname = $1 AND a.attname = $2",
			vc.table, vc.column,
		).Scan(&currentDim); err != nil {
			pool.Close()
			return nil, fmt.Errorf("check vector dim %s.%s: %w", vc.table, vc.column, err)
		}
		if currentDim == -1 {
			alterDDL := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE vector(%d)", vc.table, vc.column, vectorDim)
			if _, err := sqlDB.ExecContext(ctx, alterDDL); err != nil {
				pool.Close()
				return nil, fmt.Errorf("alter vector dim %s.%s: %w", vc.table, vc.column, err)
			}
		}
	}

	// Create HNSW indexes for vector columns (idempotent).
	hnswIndexes := []struct {
		table  string
		column string
	}{
		{"episodes", "embedding"},
		{"facts", "embedding"},
	}
	for _, idx := range hnswIndexes {
		indexName := idx.table + "_embedding_hnsw_idx"
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)", indexName,
		).Scan(&exists); err != nil {
			pool.Close()
			return nil, fmt.Errorf("check hnsw index %s: %w", indexName, err)
		}
		if !exists {
			ddl := fmt.Sprintf(
				"CREATE INDEX %s ON %s USING hnsw (%s vector_cosine_ops)",
				indexName, idx.table, idx.column,
			)
			if _, err := sqlDB.ExecContext(ctx, ddl); err != nil {
				pool.Close()
				return nil, fmt.Errorf("create hnsw index %s: %w", indexName, err)
			}
		}
	}

	// Validate dimension lock: vector dimension must match expected or be empty.
	// This allows switching between different embedding models as long as they have the same dimension.
	if err := validateDimensionLock(ctx, pool, vectorDim); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dimension lock: %w", err)
	}

	// Store embedding model metadata for audit purposes (dimension is what matters for storage).
	if err := storeEmbeddingModelMetadata(ctx, pool, expectedModel); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store embedding model metadata: %w", err)
	}

	return pool, nil
}

// validateDimensionLock ensures the vector dimension stored in the database matches the config.
// This is the actual storage constraint; the specific embedding model can vary as long as dimensions match.
func validateDimensionLock(ctx context.Context, pool *pgxpool.Pool, expectedDim int) error {
	var storedDimStr string
	err := pool.QueryRow(ctx,
		"SELECT value FROM settings WHERE key = 'vector_dimension'",
	).Scan(&storedDimStr)

	if err != nil {
		// No row yet — store the expected dimension as string.
		_, err := pool.Exec(ctx,
			"INSERT INTO settings (key, value) VALUES ('vector_dimension', $1) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()",
			fmt.Sprintf("%d", expectedDim),
		)
		return err
	}

	if storedDimStr != "" {
		storedDim, err := strconv.Atoi(storedDimStr)
		if err != nil {
			return fmt.Errorf("invalid stored dimension value: %w", err)
		}
		if storedDim != expectedDim {
			return fmt.Errorf("vector dimension mismatch: database has %d, config expects %d. You can switch between different embedding models as long as they output the same dimension. Change STASH_VECTOR_DIM to match the database, or delete the database and restart", storedDim, expectedDim)
		}
	}

	return nil
}

// storeEmbeddingModelMetadata records which embedding model is being used, for audit/monitoring purposes.
// This does not affect storage constraints (which are based on vector dimension only).
func storeEmbeddingModelMetadata(ctx context.Context, pool *pgxpool.Pool, model string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO settings (key, value) VALUES ('embedding_model', $1) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()",
		model,
	)
	return err
}
