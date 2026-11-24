// Package embedder converts text into fixed-dimension vectors.
// Implementations: OpenAI (production), Fake (tests).
package embedder

import (
	"context"
)

// Embedder converts text into a fixed-dimension vector.
// Implementations: OpenAI (production), Fake (tests).
type Embedder interface {
	// Embed generates a vector embedding for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Model returns the full model string as passed at construction.
	// Examples: "openai/text-embedding-3-small", "nomic-embed-text".
	// Used as the vector key in store.Record.Vectors.
	Model() string

	// Dims returns the vector dimensions, e.g. 1536, 768.
	Dims() int
}

// AvailabilityReporter is implemented by embedders that can report an open
// provider circuit without issuing another metered request.
type AvailabilityReporter interface {
	Availability() error
}

// Availability returns the provider state when the embedder exposes it.
func Availability(e Embedder) error {
	if reporter, ok := e.(AvailabilityReporter); ok {
		return reporter.Availability()
	}
	return nil
}
