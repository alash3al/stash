package config

import (
	"testing"
	"time"
)

// Helper function to create a base valid config for testing
func createBaseConfig() *Config {
	return &Config{
		StoreDSN:                "postgres://user:password@host:port/db",
		VectorDim:               1536, 
		MaxResultSize:           100,
		OpenAIAPIKey:            "dummy",
		OpenAIBaseURL:           "http://localhost:8080/v1",
		EmbeddingModel:          "text-embedding-ada-002",
		ReasonerModel:           "gpt-4",
		ContextTTL:              24 * time.Hour,
		HTTPAddr:                ":8080",
		LogLevel:                "info",
		LogFormat:               "json",
		DBMaxConns:              25,
		DBMinConns:              5,
		ConsolidationBatchSize:  100,
		ConsolidationSimilarityThreshold: 0.85,
		ConsolidationDedupThreshold:    0.95,
		ConsolidationWindow:          24 * time.Hour,
		DecayFactor:                  0.95,
		ExpiryThreshold:              0.1,
		HypothesisAutoConfirmThreshold: 0.95,
		HypothesisAutoRejectThreshold:  0.85,
	}
}

func TestConfig_Validate(t *testing.T) {
	// Base configuration that is generally valid
	baseConfig := createBaseConfig()

	tests := []struct {
		name          string
		modifyConfig  func(*Config)
		wantErr       bool
		expectedErrorSubstr string // Used for checking specific errors
	}{
		// --- SUCCESS CASE ---
		{
			name: "Valid configuration",
			modifyConfig: func(c *Config) {
				// No changes, ensure baseline is valid
			},
			wantErr: false,
		},
		
		// --- Test 1: VectorDim <= 0 ---
		{
			name: "Fail_VectorDim_Zero",
			modifyConfig: func(c *Config) {
				c.VectorDim = 0
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_VECTOR_DIM must be > 0",
		},
		{
			name: "Fail_VectorDim_Negative",
			modifyConfig: func(c *Config) {
				c.VectorDim = -10
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_VECTOR_DIM must be > 0",
		},

		// --- Test 2: DBMinConns >= DBMaxConns ---
		{
			name: "Fail_DBMinConns_Equals_MaxConns",
			modifyConfig: func(c *Config) {
				c.DBMinConns = 25
				c.DBMaxConns = 25
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_DB_MIN_CONNS (25) must be < STASH_DB_MAX_CONNS (25)",
		},
		{
			name: "Fail_DBMinConns_Greater_Than_MaxConns",
			modifyConfig: func(c *Config) {
				c.DBMinConns = 30
				c.DBMaxConns = 25
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_DB_MIN_CONNS (30) must be < STASH_DB_MAX_CONNS (25)",
		},
		

		// --- Test 3: ConsolidationWindow <= 0 ---
		{
			name: "Fail_ConsolidationWindow_Zero",
			modifyConfig: func(c *Config) {
				c.ConsolidationWindow = 0
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_CONSOLIDATION_WINDOW must be a positive duration",
		},
		{
			name: "Fail_ConsolidationWindow_Negative",
			modifyConfig: func(c *Config) {
				c.ConsolidationWindow = -time.Hour
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_CONSOLIDATION_WINDOW must be a positive duration",
		},
		

		// --- Test 4: SimilarityThreshold >= DedupThreshold (Deduplication order invariant) ---
		{
			name: "Fail_Similarity_Equals_Dedup",
			modifyConfig: func(c *Config) {
				c.ConsolidationSimilarityThreshold = 0.95
				c.ConsolidationDedupThreshold = 0.95
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_CONSOLIDATION_SIMILARITY_THRESHOLD (0.9500) must be < STASH_CONSOLIDATION_DEDUP_THRESHOLD (0.9500)",
		},
		{
			name: "Fail_Similarity_Greater_Than_Dedup",
			modifyConfig: func(c *Config) {
				c.ConsolidationSimilarityThreshold = 0.98
				c.ConsolidationDedupThreshold = 0.95
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_CONSOLIDATION_SIMILARITY_THRESHOLD (0.9800) must be < STASH_CONSOLIDATION_DEDUP_THRESHOLD (0.9500)",
		},
		
		// --- Test 5: ConfirmThreshold <= RejectThreshold (Ambiguous branch) ---
		{
			name: "Fail_Confirm_Equals_Reject",
			modifyConfig: func(c *Config) {
				c.HypothesisAutoConfirmThreshold = 0.85
				c.HypothesisAutoRejectThreshold = 0.85
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD (0.8500) must be > STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD (0.8500)",
		},
		{
			name: "Fail_Confirm_Less_Than_Reject",
			modifyConfig: func(c *Config) {
				c.HypothesisAutoConfirmThreshold = 0.70
				c.HypothesisAutoRejectThreshold = 0.80
			},
			wantErr: true,
			expectedErrorSubstr: "STASH_HYPOTHESIS_AUTO_CONFIRM_THRESHOLD (0.7000) must be > STASH_HYPOTHESIS_AUTO_REJECT_THRESHOLD (0.8000)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Deep copy the base config for each test run
			cfg := *baseConfig
			
			// Apply modifications
			tt.modifyConfig(&cfg)
			
			// Run validation
			err := cfg.Validate()

			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			
			if tt.wantErr && err != nil {
				if !hasSubstring(err.Error(), tt.expectedErrorSubstr) {
					t.Errorf("Validate() expected error containing '%s', but got: %s", tt.expectedErrorSubstr, err.Error())
				}
			}
		})
	}
}

// Simple substring check helper for error messages
func hasSubstring(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr
}