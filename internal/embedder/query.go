package embedder

import (
	"context"
	"fmt"
)

// Asymmetric query embedding.
//
// Some embedding models are trained ASYMMETRICALLY: a short question and the
// long passage that answers it are encoded differently on purpose. Qwen3-
// Embedding is one — it expects queries wrapped as
//
//	Instruct: {task}\nQuery: {query}
//
// while documents are embedded raw. Its model card puts the cost of omitting
// that wrapper at 1-5% retrieval performance.
//
// Symmetric models such as BAAI/bge-m3 want no wrapper at all, and adding one
// would corrupt the query vector. So the transform belongs to the EMBEDDER,
// not to the call site: callers ask for a query embedding and the configured
// model decides whether that means anything different from a document
// embedding.
//
// This is deliberately expressed as a text transform rather than a separate
// EmbedQuery method on the interface, so the existing cache path is reused
// unchanged: the cache keys on the FINAL text, which means a query vector and a
// document vector for the same words are stored under different keys and can
// never be confused for one another.

// QueryTransformer is implemented by embedders that encode queries differently
// from documents.
type QueryTransformer interface {
	// QueryText returns the text to embed when the input is a search query.
	QueryText(query string) string
}

// QueryTextFor applies the embedder's query transform, if it has one.
// Embedders without one are symmetric and get the query verbatim.
func QueryTextFor(e Embedder, query string) string {
	if qt, ok := e.(QueryTransformer); ok {
		return qt.QueryText(query)
	}
	return query
}

// EmbedQuery embeds text that is a search query rather than stored content.
//
// Use this everywhere a user's question is embedded. Using plain Embed for a
// query silently gives up the asymmetric model's advantage, and the failure is
// invisible: results still come back, just slightly worse.
func EmbedQuery(ctx context.Context, e Embedder, query string) ([]float32, error) {
	if e == nil {
		return nil, fmt.Errorf("embedder: nil embedder")
	}
	return e.Embed(ctx, QueryTextFor(e, query))
}

// FormatInstruct builds the Instruct/Query wrapper. Exported so the exact
// on-the-wire shape is testable and reviewable rather than buried in a method.
func FormatInstruct(instruction, query string) string {
	if instruction == "" {
		return query
	}
	return fmt.Sprintf("Instruct: %s\nQuery: %s", instruction, query)
}

// DefaultQueryInstruction suits a memory corpus of consolidated operational
// facts. It is a default, not a constant: the instruction is part of the
// retrieval configuration and changing it changes results, so it is worth
// tuning against the gold eval rather than guessing once.
const DefaultQueryInstruction = "Given a question, retrieve the stored facts that answer it"
