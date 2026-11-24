package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// Cached wraps an Embedder with pgx-backed caching and request deduplication.
type Cached struct {
	embedder Embedder
	pool     *pgxpool.Pool
	inflight sync.Map
}

type call struct {
	wg  sync.WaitGroup
	vec []float32
	err error
}

// NewCached creates a cached embedder that stores embeddings in the embedding_cache table.
func NewCached(e Embedder, pool *pgxpool.Pool) *Cached {
	return &Cached{
		embedder: e,
		pool:     pool,
	}
}

// Embed returns a cached embedding or calls the underlying embedder.
// Deduplicates concurrent requests for the same text.
func (c *Cached) Embed(ctx context.Context, text string) ([]float32, error) {
	hash := cacheKey(text)

	// Try cache
	cached, err := c.getCached(ctx, hash, c.embedder.Model())
	if err == nil && cached != nil {
		return cached, nil
	}

	// Check inflight dedup
	callVal, loaded := c.inflight.LoadOrStore(hash, &call{})
	callInfo := callVal.(*call)

	if loaded {
		callInfo.wg.Wait()
		return callInfo.vec, callInfo.err
	}

	callInfo.wg.Add(1)
	defer func() {
		callInfo.wg.Done()
		c.inflight.Delete(hash)
	}()

	vec, err := c.embedder.Embed(ctx, text)
	if err != nil {
		callInfo.err = err
		return nil, err
	}

	callInfo.vec = vec

	// Write cache in background with timeout
	cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.putCached(cacheCtx, hash, text, vec, c.embedder.Model()); err != nil {
		log.Printf("embedder: cache write failed for hash %s: %v", hash[:8], err)
	}

	return vec, nil
}

// Model returns the underlying embedder's model.
func (c *Cached) Model() string {
	return c.embedder.Model()
}

// Dims returns the underlying embedder's dimensions.
func (c *Cached) Dims() int {
	return c.embedder.Dims()
}

// QueryText forwards the wrapped embedder's asymmetric query transform.
//
// Forwarding matters: without it the type assertion in QueryTextFor fails on
// the cache wrapper and every query is silently embedded as a document — a
// regression that produces slightly worse results and no error at all.
//
// The cache needs no other change. Embed keys on the transformed text, so a
// query vector and a document vector for the same words land under different
// keys and can never be served for one another.
func (c *Cached) QueryText(query string) string {
	return QueryTextFor(c.embedder, query)
}

// Availability delegates provider circuit state through the cache wrapper.
func (c *Cached) Availability() error {
	return Availability(c.embedder)
}

func (c *Cached) getCached(ctx context.Context, hash, model string) ([]float32, error) {
	var vec pgvector.Vector
	err := c.pool.QueryRow(ctx,
		"SELECT embedding FROM embedding_cache WHERE text_hash = $1 AND model = $2",
		hash, model,
	).Scan(&vec)
	if err != nil {
		return nil, err
	}
	return vec.Slice(), nil
}

func (c *Cached) putCached(ctx context.Context, hash, text string, vec []float32, model string) error {
	_, err := c.pool.Exec(ctx,
		`INSERT INTO embedding_cache (text_hash, model, text, embedding)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (text_hash, model) DO NOTHING`,
		hash, model, text, pgvector.NewVector(vec),
	)
	return err
}

func cacheKey(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:])
}
