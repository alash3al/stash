package embedder

import (
	"context"
	"strings"
	"testing"
)

// symmetric has no QueryText method — the shape of a model like bge-m3.
type symmetric struct{ last string }

func (s *symmetric) Embed(_ context.Context, text string) ([]float32, error) {
	s.last = text
	return []float32{1}, nil
}
func (s *symmetric) Model() string { return "symmetric/model" }
func (s *symmetric) Dims() int     { return 1 }

// asymmetric wraps queries, like Qwen3-Embedding.
type asymmetric struct {
	symmetric
	instruction string
}

func (a *asymmetric) QueryText(q string) string { return FormatInstruct(a.instruction, q) }

func TestSymmetricModelsGetTheQueryVerbatim(t *testing.T) {
	// Wrapping a query for a model not trained on the wrapper corrupts the
	// vector, so the default must be to change nothing.
	e := &symmetric{}
	if _, err := EmbedQuery(context.Background(), e, "how do deploys work"); err != nil {
		t.Fatal(err)
	}
	if e.last != "how do deploys work" {
		t.Fatalf("symmetric embedder received %q, want the query unchanged", e.last)
	}
}

func TestAsymmetricModelsGetTheInstructWrapper(t *testing.T) {
	e := &asymmetric{instruction: "Given a question, retrieve the stored facts that answer it"}
	if _, err := EmbedQuery(context.Background(), e, "how do deploys work"); err != nil {
		t.Fatal(err)
	}
	want := "Instruct: Given a question, retrieve the stored facts that answer it\nQuery: how do deploys work"
	if e.last != want {
		t.Fatalf("got %q\nwant %q", e.last, want)
	}
}

func TestEmptyInstructionIsByteIdenticalToTheOldBehaviour(t *testing.T) {
	// This is the safety property for the LIVE path: turning the feature on
	// with no instruction configured must not change a single byte of what
	// gets embedded, so wiring it into recall is a provable no-op.
	e := &asymmetric{instruction: ""}
	if got := e.QueryText("unchanged text"); got != "unchanged text" {
		t.Fatalf("empty instruction altered the query: %q", got)
	}
	if got := FormatInstruct("", "unchanged text"); got != "unchanged text" {
		t.Fatalf("FormatInstruct with no instruction altered the query: %q", got)
	}
}

func TestDocumentsAreNeverWrapped(t *testing.T) {
	// Only queries are asymmetric. Wrapping stored content would make every
	// document vector incompatible with the corpus already embedded.
	e := &asymmetric{instruction: "some instruction"}
	if _, err := e.Embed(context.Background(), "a stored fact"); err != nil {
		t.Fatal(err)
	}
	if e.last != "a stored fact" {
		t.Fatalf("document text was transformed to %q; documents must be embedded raw", e.last)
	}
	if strings.Contains(e.last, "Instruct:") {
		t.Fatal("document embedding must never carry the Instruct wrapper")
	}
}

func TestQueryTextForFallsBackWhenTheModelIsSymmetric(t *testing.T) {
	if got := QueryTextFor(&symmetric{}, "q"); got != "q" {
		t.Fatalf("QueryTextFor = %q, want %q", got, "q")
	}
}

func TestWrapperFormatMatchesTheModelCard(t *testing.T) {
	// Qwen specifies exactly: "Instruct: {task}\nQuery: {query}".
	// A stray space or a \r\n here silently degrades every query.
	got := FormatInstruct("T", "Q")
	if got != "Instruct: T\nQuery: Q" {
		t.Fatalf("wrapper = %q, want %q", got, "Instruct: T\nQuery: Q")
	}
}

func TestEmbedQueryRejectsANilEmbedder(t *testing.T) {
	if _, err := EmbedQuery(context.Background(), nil, "q"); err == nil {
		t.Fatal("nil embedder must error, not panic")
	}
}
