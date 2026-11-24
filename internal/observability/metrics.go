package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	eventsProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_events_processed_total",
			Help: "Total number of events processed by consolidation",
		}, []string{"namespace"},
	)
	factsCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_facts_created_total",
			Help: "Total number of facts created during consolidation",
		}, []string{"namespace"},
	)
	factsDeduplicated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_facts_deduplicated_total",
			Help: "Total number of facts skipped due to semantic deduplication",
		}, []string{"namespace"},
	)
	relationshipsCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_relationships_created_total",
			Help: "Total number of relationships extracted",
		}, []string{"namespace"},
	)
	llmCalls = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_llm_calls_total",
			Help: "Total number of LLM calls made during consolidation",
		}, []string{"namespace"},
	)
	clustersFound = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_clusters_found_total",
			Help: "Total number of clusters evaluated",
		}, []string{"namespace"},
	)
	eventsRead = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_events_read_total",
			Help: "Total number of events read during consolidation",
		}, []string{"namespace"},
	)
	duration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "consolidation_duration_seconds",
			Help:    "Duration of consolidation runs",
			Buckets: prometheus.DefBuckets,
		}, []string{"namespace"},
	)
	errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "consolidation_errors_total",
			Help: "Number of errors encountered during consolidation",
		}, []string{"namespace"},
	)
	recallRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "stash_recall_requests_total",
			Help: "Total recall requests by learning mode",
		}, []string{"learning"},
	)
	recallResults = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "stash_recall_results",
			Help:    "Number of results returned per recall",
			Buckets: []float64{0, 1, 3, 5, 10, 25, 50, 100},
		},
	)
	recallFeedback = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "stash_recall_feedback_total",
			Help: "Idempotent recall feedback events by signal",
		}, []string{"signal"},
	)
	recallLearningErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "stash_recall_learning_errors_total",
			Help: "Non-fatal recall learning ledger errors by stage",
		}, []string{"stage"},
	)
	deferredEmbeddings = promauto.NewCounterVec(
		prometheus.CounterOpts{Name: "stash_deferred_embeddings_total", Help: "Writes preserved without an embedding by memory type"},
		[]string{"memory_type"},
	)
	degradedRecalls = promauto.NewCounter(
		prometheus.CounterOpts{Name: "stash_degraded_recalls_total", Help: "Recall requests served by PostgreSQL lexical fallback"},
	)
	degradedRecallResults = promauto.NewHistogram(
		prometheus.HistogramOpts{Name: "stash_degraded_recall_results", Help: "Number of lexical results returned during degraded recall", Buckets: []float64{0, 1, 3, 5, 10, 25, 50, 100}},
	)
)

// Observation carries the metrics that should be exported for a run.
type Observation struct {
	Namespace          string
	EventsRead         int
	EventsProcessed    int
	ClustersFound      int
	FactsCreated       int
	FactsDeduplicated  int
	RelationshipsFound int
	LLMCalls           int
	Duration           time.Duration
	Errors             int
}

// RecordConsolidation exports the provided observation to Prometheus.
func RecordConsolidation(obs Observation) {
	if obs.Namespace == "" {
		obs.Namespace = "default"
	}
	eventsProcessed.WithLabelValues(obs.Namespace).Add(float64(obs.EventsProcessed))
	eventsRead.WithLabelValues(obs.Namespace).Add(float64(obs.EventsRead))
	clustersFound.WithLabelValues(obs.Namespace).Add(float64(obs.ClustersFound))
	factsCreated.WithLabelValues(obs.Namespace).Add(float64(obs.FactsCreated))
	factsDeduplicated.WithLabelValues(obs.Namespace).Add(float64(obs.FactsDeduplicated))
	relationshipsCreated.WithLabelValues(obs.Namespace).Add(float64(obs.RelationshipsFound))
	llmCalls.WithLabelValues(obs.Namespace).Add(float64(obs.LLMCalls))
	duration.WithLabelValues(obs.Namespace).Observe(obs.Duration.Seconds())
	errorsTotal.WithLabelValues(obs.Namespace).Add(float64(obs.Errors))
}

func RecordRecall(resultCount int, learning bool) {
	mode := "disabled"
	if learning {
		mode = "enabled"
	}
	recallRequests.WithLabelValues(mode).Inc()
	recallResults.Observe(float64(resultCount))
}

func RecordRecallFeedback(signal string) {
	recallFeedback.WithLabelValues(signal).Inc()
}

func RecordRecallLearningError(stage string) {
	recallLearningErrors.WithLabelValues(stage).Inc()
}

func RecordDeferredEmbedding(memoryType string) {
	deferredEmbeddings.WithLabelValues(memoryType).Inc()
}

func RecordDegradedRecall(resultCount int) {
	degradedRecalls.Inc()
	degradedRecallResults.Observe(float64(resultCount))
}
