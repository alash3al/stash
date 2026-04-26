# Configuration Validation Invariants

The `Validate()` function in `internal/config/config.go` protects against the following 5 configuration invariants:

- **Vector dimension must be positive**: `STASH_VECTOR_DIM` must be strictly greater than 0. A zero or negative dimension would break embedding operations and vector similarity calculations.

- **Connection pool bounds**: `STASH_DB_MIN_CONNS` (minimum pool size) must be strictly less than `STASH_DB_MAX_CONNS` (maximum pool size). If min >= max, the connection pool cannot function as designed and may cause resource exhaustion or starvation.

- **Consolidation window validity**: `STASH_CONSOLIDATION_WINDOW` must be a positive duration. This window defines the timeframe for episode consolidation; a zero or negative value would break time-bounded processing logic.

- **Deduplication order invariant**: `STASH_CONSOLIDATION_SIMILARITY_THRESHOLD` must be strictly less than `STASH_CONSOLIDATION_DEDUP_THRESHOLD`. The deduplication step fires after similarity grouping, so the dedup threshold must be stricter (higher) to avoid incorrect merges before grouping can separate duplicates.

- **Hypothesis decision branch**: `STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD` must be strictly greater than `STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD`. Equal or inverted thresholds create an ambiguous branch where a single confidence score could qualify for both auto-confirmation and auto-rejection simultaneously, breaking deterministic behavior.
