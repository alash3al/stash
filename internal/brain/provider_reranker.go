package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ProviderReranker is deliberately small so recall stays available when an
// optional external ranking service is unavailable.
type ProviderReranker interface {
	Rerank(ctx context.Context, query string, documents []string) ([]float32, error)
}

type deepInfraReranker struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewDeepInfraReranker(baseURL, apiKey, model string) (ProviderReranker, error) {
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("brain: DeepInfra reranker requires API key and model")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.deepinfra.com"
	}
	return &deepInfraReranker{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (r *deepInfraReranker) Rerank(ctx context.Context, query string, documents []string) ([]float32, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	queries := make([]string, len(documents))
	for i := range queries {
		queries[i] = query
	}
	body, err := json.Marshal(struct {
		Queries   []string `json:"queries"`
		Documents []string `json:"documents"`
	}{Queries: queries, Documents: documents})
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/inference/"+r.model, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build rerank request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request rerank: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("DeepInfra reranker returned HTTP %d", resp.StatusCode)
	}
	var decoded struct {
		Scores []float32 `json:"scores"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode rerank response: %w", err)
	}
	if len(decoded.Scores) != len(documents) {
		return nil, fmt.Errorf("DeepInfra reranker returned %d scores for %d documents", len(decoded.Scores), len(documents))
	}
	return decoded.Scores, nil
}
