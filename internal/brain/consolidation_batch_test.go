package brain

import "testing"

func TestConsolidationBatchLimit(t *testing.T) {
	tests := []struct {
		configured int
		maximum    int
		want       int
	}{
		{configured: 10, maximum: 50, want: 10},
		{configured: 100, maximum: 30, want: 30},
		{configured: 5, maximum: 0, want: 5},
		{configured: 0, maximum: 50, want: 50},
	}
	for _, test := range tests {
		brain := &Brain{config: Config{BatchSize: test.configured}}
		if got := brain.consolidationBatchLimit(test.maximum); got != test.want {
			t.Fatalf("configured=%d maximum=%d: got %d, want %d", test.configured, test.maximum, got, test.want)
		}
	}
}

func TestCausalBatchHasMinimumPairSize(t *testing.T) {
	brain := &Brain{config: Config{BatchSize: 1}}
	causalLimit := brain.consolidationBatchLimitAtLeast(30, 2)
	if causalLimit != 2 {
		t.Fatalf("causal limit = %d, want 2", causalLimit)
	}
}
