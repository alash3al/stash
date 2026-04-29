package brain

import (
	"context"
	"strings"
	"testing"
)

func TestNamespacePathLength(t *testing.T) {
	b := &Brain{} // Minimal mock for validation test
	ctx := context.Background()

	// 1. Create a 300-character slug
	longSlug := "/" + strings.Repeat("a", 299)

	// 2. Try to create a namespace with it
	_, err := b.CreateNamespace(ctx, longSlug, "Too Long", "Testing length limit")

	// 3. Verify it fails with the correct error
	if err == nil {
		t.Fatal("expected error for 300-char slug, got nil")
	}

	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected 'too long' error, got: %v", err)
	}
}
