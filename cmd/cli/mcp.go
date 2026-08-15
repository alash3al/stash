package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/alash3al/stash/internal/bootstrap"
	"github.com/alash3al/stash/internal/brain"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/urfave/cli/v3"
)

//go:embed mcp_prompts.tmpl
var mcpPromptsFS string

var mcpTmpl *template.Template

func init() {
	var err error
	mcpTmpl, err = template.New("mcp_prompts").Parse(mcpPromptsFS)
	if err != nil {
		panic(fmt.Sprintf("parse mcp_prompts.tmpl: %v", err))
	}
}

func render(name string) string {
	var buf strings.Builder
	if err := mcpTmpl.ExecuteTemplate(&buf, name, nil); err != nil {
		return ""
	}
	return buf.String()
}

func newMCPServer(bc *bootstrap.Context) *server.MCPServer {
	mcpServer := server.NewMCPServer("stash", "0.2.8",
		server.WithToolCapabilities(true),
		server.WithDescription(render("server_description")),
	)

	mcpServer.AddTool(mcp.NewTool("init",
		mcp.WithDescription(render("init_description")),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		scaffold := []struct {
			slug        string
			name        string
			description string
		}{
			{"/self", "Self", "Agent's self-knowledge: capabilities, limits, preferences, and behavioral patterns."},
			{"/self/capabilities", "Capabilities", "What the agent can do well. Strengths and proven competencies."},
			{"/self/limits", "Limits", "What the agent struggles with or cannot do. Known failure modes."},
			{"/self/preferences", "Preferences", "How the agent works best. Behavioral patterns and standing instructions."},
		}

		var created []string
		for _, ns := range scaffold {
			_, err := bc.Brain.CreateNamespace(ctx, ns.slug, ns.name, ns.description)
			if err != nil {
				return nil, fmt.Errorf("init: create namespace %s: %w", ns.slug, err)
			}
			created = append(created, ns.slug)
		}

		result := map[string]any{"ok": true, "namespaces_created": created}
		b, _ := json.Marshal(result)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("remember",
		mcp.WithDescription(render("remember_description")),
		mcp.WithString("content", mcp.Description(render("remember_content")), mcp.Required()),
		mcp.WithString("namespace", mcp.Description(render("remember_namespace"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content := request.GetString("content", "")
		namespace := request.GetString("namespace", "/")

		id, err := bc.Brain.Remember(ctx, namespace, content, nil)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"id": id, "message": "Memory remembered successfully"}
		b, _ := json.Marshal(result)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("recall",
		mcp.WithDescription(render("recall_description")),
		mcp.WithString("query", mcp.Description(render("recall_query")), mcp.Required()),
		mcp.WithString("namespaces", mcp.Description(render("recall_namespaces"))),
		mcp.WithNumber("limit", mcp.Description(render("limit_param")), mcp.DefaultNumber(10)),
		mcp.WithBoolean("learn", mcp.Description(render("recall_learn")), mcp.DefaultBool(true)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := request.GetString("query", "")
		nsRaw := request.GetString("namespaces", "/")
		limit := request.GetInt("limit", 10)
		learn := request.GetBool("learn", true)

		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		results, err := bc.Brain.RecallWithOptions(ctx, namespaces, query, limit, brain.RecallOptions{
			RecordOutcome: learn,
			Caller:        "mcp",
		})
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(results)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("record_recall_feedback",
		mcp.WithDescription(render("record_recall_feedback_description")),
		mcp.WithNumber("impression_id", mcp.Description(render("record_recall_feedback_impression_id")), mcp.Required()),
		mcp.WithString("memory_type", mcp.Description(render("record_recall_feedback_memory_type")), mcp.Required()),
		mcp.WithNumber("memory_id", mcp.Description(render("record_recall_feedback_memory_id")), mcp.Required()),
		mcp.WithString("signal", mcp.Description(render("record_recall_feedback_signal")), mcp.Required()),
		mcp.WithString("idempotency_key", mcp.Description(render("record_recall_feedback_idempotency_key")), mcp.Required()),
		mcp.WithString("reason", mcp.Description(render("record_recall_feedback_reason"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := bc.Brain.RecordRecallFeedback(
			ctx,
			int64(request.GetInt("impression_id", 0)),
			request.GetString("memory_type", ""),
			int64(request.GetInt("memory_id", 0)),
			request.GetString("signal", ""),
			request.GetString("idempotency_key", ""),
			request.GetString("reason", ""),
		)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(result)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("forget",
		mcp.WithDescription(render("forget_description")),
		mcp.WithString("about", mcp.Description(render("forget_about")), mcp.Required()),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		about := request.GetString("about", "")
		nsRaw := request.GetString("namespaces", "/")

		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		if err := bc.Brain.ForgetEpisode(ctx, namespaces, about); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"ok": true}`}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("consolidate",
		mcp.WithDescription(render("consolidate_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		ids, err := bc.Brain.ResolveNamespaceIDs(ctx, namespaces)
		if err != nil {
			return nil, err
		}

		var summaries []map[string]any
		for _, id := range ids {
			result, err := bc.Brain.ConsolidateByID(ctx, id)
			if err != nil {
				summaries = append(summaries, map[string]any{"namespace_id": id, "error": err.Error()})
				continue
			}
			summaries = append(summaries, map[string]any{
				"namespace":                    result.Namespace,
				"episodes_read":                result.EpisodesRead,
				"facts_created":                result.FactsCreated,
				"facts_deduplicated":           result.FactsDeduplicated,
				"relationships_found":          result.RelationshipsFound,
				"causal_links_found":           result.CausalLinksFound,
				"patterns_found":               result.PatternsFound,
				"contradictions_found":         result.ContradictionsFound,
				"contradictions_auto_resolved": result.ContradictionsAutoResolved,
				"goals_annotated":              result.GoalsAnnotated,
				"goals_suggested_complete":     result.GoalsSuggestedComplete,
				"failure_repeats_detected":     result.FailureRepeatsDetected,
				"failure_patterns_found":       result.FailurePatternsFound,
				"hypotheses_auto_confirmed":    result.HypothesesAutoConfirmed,
				"hypotheses_auto_rejected":     result.HypothesesAutoRejected,
				"hypotheses_updated":           result.HypothesesUpdated,
				"facts_decayed":                result.FactsDecayed,
				"facts_expired":                result.FactsExpired,
				"llm_calls":                    result.LLMCalls,
				"duration":                     result.Duration.String(),
				"errors":                       result.Errors,
			})
		}
		b, _ := json.Marshal(summaries)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("set_context",
		mcp.WithDescription(render("set_context_description")),
		mcp.WithString("focus", mcp.Description(render("set_context_focus")), mcp.Required()),
		mcp.WithString("namespace", mcp.Description(render("context_namespace_param")), mcp.Required()),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		focus := request.GetString("focus", "")
		namespace := request.GetString("namespace", "")
		if namespace == "" {
			return nil, fmt.Errorf("namespace is required for context tools")
		}
		if err := bc.Brain.SetContext(ctx, namespace, focus, time.Now().UTC().Add(1*time.Hour)); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"ok": true}`}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("get_context",
		mcp.WithDescription(render("get_context_description")),
		mcp.WithString("namespace", mcp.Description(render("context_namespace_param")), mcp.Required()),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		namespace := request.GetString("namespace", "")
		if namespace == "" {
			return nil, fmt.Errorf("namespace is required for context tools")
		}
		c, err := bc.Brain.GetContext(ctx, namespace)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"focus": ""}`}}}, nil
		}
		b, _ := json.Marshal(c)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("clear_context",
		mcp.WithDescription(render("clear_context_description")),
		mcp.WithString("namespace", mcp.Description(render("context_namespace_param")), mcp.Required()),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		namespace := request.GetString("namespace", "")
		if namespace == "" {
			return nil, fmt.Errorf("namespace is required for context tools")
		}
		if err := bc.Brain.ClearContext(ctx, namespace); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"ok": true}`}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_namespaces",
		mcp.WithDescription(render("list_namespaces_description")),
		mcp.WithString("namespaces", mcp.Description(render("list_namespaces_namespaces"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "")
		var patterns []string
		if nsRaw != "" {
			for _, ns := range strings.Split(nsRaw, ",") {
				if ns = strings.TrimSpace(ns); ns != "" {
					patterns = append(patterns, ns)
				}
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		namespaces, err := bc.Brain.ListNamespaces(ctx, patterns, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(namespaces)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("create_namespace",
		mcp.WithDescription(render("create_namespace_description")),
		mcp.WithString("slug", mcp.Description(render("create_namespace_slug")), mcp.Required()),
		mcp.WithString("name", mcp.Description(render("create_namespace_name")), mcp.Required()),
		mcp.WithString("description", mcp.Description(render("create_namespace_description_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug := request.GetString("slug", "")
		name := request.GetString("name", slug)
		description := request.GetString("description", "")

		id, err := bc.Brain.CreateNamespace(ctx, slug, name, description)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{
			Type: "text",
			Text: fmt.Sprintf(`{"id": %d, "slug": "%s"}`, id, slug),
		}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("query_facts",
		mcp.WithDescription(render("query_facts_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		facts, err := bc.Brain.QueryFacts(ctx, namespaces, nil, nil, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(facts)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("query_relationships",
		mcp.WithDescription(render("query_relationships_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		rels, err := bc.Brain.QueryRelationships(ctx, namespaces, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(rels)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_contradictions",
		mcp.WithDescription(render("list_contradictions_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		contradictions, err := bc.Brain.ListContradictions(ctx, namespaces, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(contradictions)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("resolve_contradiction",
		mcp.WithDescription(render("resolve_contradiction_description")),
		mcp.WithNumber("id", mcp.Description(render("resolve_contradiction_id")), mcp.Required()),
		mcp.WithString("resolution", mcp.Description(render("resolve_contradiction_resolution"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)
		resolution := request.GetString("resolution", "resolved")

		if err := bc.Brain.ResolveContradiction(ctx, int64(id), resolution); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"ok": true}`}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_causal_links",
		mcp.WithDescription(render("list_causal_links_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		links, err := bc.Brain.ListCausalLinks(ctx, namespaces, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(links)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("create_causal_link",
		mcp.WithDescription(render("create_causal_link_description")),
		mcp.WithNumber("cause_id", mcp.Description(render("create_causal_link_cause_id")), mcp.Required()),
		mcp.WithNumber("effect_id", mcp.Description(render("create_causal_link_effect_id")), mcp.Required()),
		mcp.WithNumber("confidence", mcp.Description(render("create_causal_link_confidence")), mcp.DefaultNumber(0.8)),
		mcp.WithString("namespace", mcp.Description(render("namespace_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		causeID := request.GetInt("cause_id", 0)
		effectID := request.GetInt("effect_id", 0)
		confidence := float32(request.GetFloat("confidence", 0.8))
		namespace := request.GetString("namespace", "/")

		nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
		if err != nil {
			return nil, err
		}

		link, err := bc.Brain.CreateCausalLink(ctx, nsIDs[0], int64(causeID), int64(effectID), confidence)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(link)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("trace_causal_chain",
		mcp.WithDescription(render("trace_causal_chain_description")),
		mcp.WithNumber("fact_id", mcp.Description(render("trace_causal_chain_fact_id")), mcp.Required()),
		mcp.WithString("direction", mcp.Description(render("trace_causal_chain_direction")), mcp.DefaultString("forward")),
		mcp.WithNumber("max_depth", mcp.Description(render("trace_causal_chain_max_depth")), mcp.DefaultNumber(10)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		factID := request.GetInt("fact_id", 0)
		direction := request.GetString("direction", "forward")
		maxDepth := request.GetInt("max_depth", 10)

		chain, err := bc.Brain.TraceCausalChain(ctx, int64(factID), direction, maxDepth)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(chain)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_hypotheses",
		mcp.WithDescription(render("list_hypotheses_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithString("status", mcp.Description(render("list_hypotheses_status"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}
		status := request.GetString("status", "")

		hypotheses, err := bc.Brain.ListHypotheses(ctx, namespaces, status, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(hypotheses)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("create_hypothesis",
		mcp.WithDescription(render("create_hypothesis_description")),
		mcp.WithString("content", mcp.Description(render("create_hypothesis_content")), mcp.Required()),
		mcp.WithString("verification_plan", mcp.Description(render("create_hypothesis_verification_plan")), mcp.Required()),
		mcp.WithNumber("confidence", mcp.Description(render("create_hypothesis_confidence")), mcp.DefaultNumber(0.5)),
		mcp.WithString("namespace", mcp.Description(render("namespace_param"))),
		mcp.WithString("source_fact_ids", mcp.Description(render("create_hypothesis_source_fact_ids"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content := request.GetString("content", "")
		verificationPlan := request.GetString("verification_plan", "")
		confidence := float32(request.GetFloat("confidence", 0.5))
		namespace := request.GetString("namespace", "/")

		var sourceFactIDs []int64
		if raw := request.GetString("source_fact_ids", ""); raw != "" {
			for _, s := range strings.Split(raw, ",") {
				if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					sourceFactIDs = append(sourceFactIDs, id)
				}
			}
		}

		nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
		if err != nil {
			return nil, err
		}

		h, err := bc.Brain.CreateHypothesis(ctx, nsIDs[0], content, verificationPlan, confidence, sourceFactIDs)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(h)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("confirm_hypothesis",
		mcp.WithDescription(render("confirm_hypothesis_description")),
		mcp.WithNumber("id", mcp.Description(render("confirm_hypothesis_id")), mcp.Required()),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)

		h, f, err := bc.Brain.ConfirmHypothesis(ctx, int64(id))
		if err != nil {
			return nil, err
		}
		result := map[string]any{
			"hypothesis": h,
			"fact":       f,
		}
		b, _ := json.Marshal(result)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("reject_hypothesis",
		mcp.WithDescription(render("reject_hypothesis_description")),
		mcp.WithNumber("id", mcp.Description(render("reject_hypothesis_id")), mcp.Required()),
		mcp.WithString("reason", mcp.Description(render("reject_hypothesis_reason"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)
		reason := request.GetString("reason", "")

		h, err := bc.Brain.RejectHypothesis(ctx, int64(id), reason)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(h)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_goals",
		mcp.WithDescription(render("list_goals_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithString("status", mcp.Description(render("list_goals_status"))),
		mcp.WithNumber("parent_id", mcp.Description(render("list_goals_parent_id"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}
		status := request.GetString("status", "")

		var parentID *int64
		if pid := request.GetInt("parent_id", 0); pid != 0 {
			pid64 := int64(pid)
			parentID = &pid64
		}

		goals, err := bc.Brain.ListGoals(ctx, namespaces, status, parentID, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(goals)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("create_goal",
		mcp.WithDescription(render("create_goal_description")),
		mcp.WithString("content", mcp.Description(render("create_goal_content")), mcp.Required()),
		mcp.WithNumber("parent_id", mcp.Description(render("create_goal_parent_id"))),
		mcp.WithNumber("priority", mcp.Description(render("create_goal_priority")), mcp.DefaultNumber(0)),
		mcp.WithString("namespace", mcp.Description(render("namespace_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content := request.GetString("content", "")
		priority := request.GetInt("priority", 0)
		namespace := request.GetString("namespace", "/")

		var parentID *int64
		if pid := request.GetInt("parent_id", 0); pid != 0 {
			pid64 := int64(pid)
			parentID = &pid64
		}

		nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
		if err != nil {
			return nil, err
		}

		g, err := bc.Brain.CreateGoal(ctx, nsIDs[0], content, parentID, priority)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(g)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("complete_goal",
		mcp.WithDescription(render("complete_goal_description")),
		mcp.WithNumber("id", mcp.Description(render("complete_goal_id")), mcp.Required()),
		mcp.WithString("notes", mcp.Description(render("complete_goal_notes"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)
		notes := request.GetString("notes", "")

		g, err := bc.Brain.CompleteGoal(ctx, int64(id), notes)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(g)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("abandon_goal",
		mcp.WithDescription(render("abandon_goal_description")),
		mcp.WithNumber("id", mcp.Description(render("abandon_goal_id")), mcp.Required()),
		mcp.WithString("notes", mcp.Description(render("abandon_goal_notes"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)
		notes := request.GetString("notes", "")

		g, err := bc.Brain.AbandonGoal(ctx, int64(id), notes)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(g)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("list_failures",
		mcp.WithDescription(render("list_failures_description")),
		mcp.WithString("namespaces", mcp.Description(render("namespaces_param"))),
		mcp.WithNumber("goal_id", mcp.Description(render("list_failures_goal_id"))),
		mcp.WithNumber("limit", mcp.Description(render("pagination_limit")), mcp.DefaultNumber(100)),
		mcp.WithNumber("offset", mcp.Description(render("pagination_offset")), mcp.DefaultNumber(0)),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsRaw := request.GetString("namespaces", "/")
		var namespaces []string
		for _, ns := range strings.Split(nsRaw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}

		page := brain.Pagination{
			Limit:  request.GetInt("limit", 100),
			Offset: request.GetInt("offset", 0),
		}

		var goalID *int64
		if gid := request.GetInt("goal_id", 0); gid != 0 {
			gid64 := int64(gid)
			goalID = &gid64
		}

		failures, err := bc.Brain.ListFailures(ctx, namespaces, goalID, page)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(failures)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("create_failure",
		mcp.WithDescription(render("create_failure_description")),
		mcp.WithString("content", mcp.Description(render("create_failure_content")), mcp.Required()),
		mcp.WithString("reason", mcp.Description(render("create_failure_reason")), mcp.Required()),
		mcp.WithString("lesson", mcp.Description(render("create_failure_lesson")), mcp.Required()),
		mcp.WithNumber("goal_id", mcp.Description(render("create_failure_goal_id"))),
		mcp.WithString("namespace", mcp.Description(render("namespace_param"))),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		content := request.GetString("content", "")
		reason := request.GetString("reason", "")
		lesson := request.GetString("lesson", "")
		namespace := request.GetString("namespace", "/")

		var goalID *int64
		if gid := request.GetInt("goal_id", 0); gid != 0 {
			gid64 := int64(gid)
			goalID = &gid64
		}

		nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
		if err != nil {
			return nil, err
		}

		f, err := bc.Brain.CreateFailure(ctx, nsIDs[0], content, reason, lesson, goalID)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(f)
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}}}, nil
	})

	mcpServer.AddTool(mcp.NewTool("delete_failure",
		mcp.WithDescription(render("delete_failure_description")),
		mcp.WithNumber("id", mcp.Description(render("delete_failure_id")), mcp.Required()),
	), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := request.GetInt("id", 0)

		if err := bc.Brain.DeleteFailure(ctx, int64(id)); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: `{"ok": true}`}}}, nil
	})

	return mcpServer
}

func mcpServeCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	mcpServer := newMCPServer(bc)
	sseServer := server.NewSSEServer(mcpServer)
	// Modern Streamable HTTP transport (MCP spec >= 2025-03-26), served at /mcp
	// ALONGSIDE the legacy SSE transport. Stateless keeps each request
	// self-contained, which is simplest and most robust behind a reverse proxy.
	streamableServer := server.NewStreamableHTTPServer(mcpServer, server.WithStateLess(true))

	host := cmd.String("host")
	port := cmd.String("port")
	addr := host + ":" + port

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup

	if cmd.Bool("with-consolidation") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConsolidationTicker(ctx, bc, cmd)
		}()
	}

	// One listener, both transports. Mounting the whole SSEServer at "/" keeps
	// /sse and /message byte-for-byte identical to the previous behavior
	// (SSEServer.Start also used the SSEServer as the sole HTTP handler); "/mcp"
	// is the only added route.
	//
	// /healthz is ALSO added here (mirroring server.go's real bc.Brain.Health
	// check, not a bare process-up ping) because deploy platforms like Railway
	// health-check a service's primary/detected port — which is this one
	// (mcp-port), not the separate metrics port (http-port) serveHTTP() listens
	// on. Without this, an external healthcheck aimed at this port 404s
	// regardless of whether the database is actually reachable.
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamableServer)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := bc.Brain.Health(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := bc.Brain.Ready(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/", sseServer)
	httpServer := &http.Server{Addr: addr, Handler: mux}

	fmt.Printf("Starting MCP server (SSE at /sse, Streamable HTTP at /mcp) on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		fmt.Println("\nMCP server shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
		wg.Wait()
		return nil
	case err := <-errCh:
		cancel()
		wg.Wait()
		return err
	}
}

func mcpExecuteCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	mcpServer := newMCPServer(bc)

	var wg sync.WaitGroup

	if cmd.Bool("with-consolidation") {
		ctx2, cancel := context.WithCancel(ctx)
		defer cancel()
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConsolidationTicker(ctx2, bc, cmd)
		}()
	}

	err := server.ServeStdio(mcpServer)
	wg.Wait()
	return err
}

func runConsolidationTicker(ctx context.Context, bc *bootstrap.Context, cmd *cli.Command) {
	interval := cmd.Duration("consolidate-interval")
	namespaces := cmd.StringSlice("consolidate-namespaces")

	if len(namespaces) == 0 {
		namespaces = []string{"/"}
	}

	log.Printf("Starting background consolidation with interval %s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if updated, err := bc.Brain.BackfillMissingEmbeddings(ctx); err != nil {
				log.Printf("Embedding backfill deferred: %v", err)
			} else if updated > 0 {
				log.Printf("Embedding backfill updated %d memories", updated)
			}
			ids, err := bc.Brain.ResolveNamespaceIDs(ctx, namespaces)
			if err != nil {
				log.Printf("Consolidation: failed to resolve namespaces: %v", err)
				continue
			}
			for _, id := range ids {
				result, err := bc.Brain.ConsolidateByID(ctx, id)
				if err != nil {
					log.Printf("Consolidation failed for namespace ID %d: %v", id, err)
					continue
				}
				log.Printf("Consolidation completed for %s: facts=%d relationships=%d goals_annotated=%d failure_repeats=%d hypotheses_updated=%d",
					result.Namespace, result.FactsCreated, result.RelationshipsFound, result.GoalsAnnotated, result.FailureRepeatsDetected, result.HypothesesUpdated)
			}
			if deleted, err := bc.Brain.PruneRecallHistory(ctx); err != nil {
				log.Printf("Recall history pruning failed: %v", err)
			} else if deleted > 0 {
				log.Printf("Recall history pruning removed %d expired impressions", deleted)
			}
		case <-ctx.Done():
			log.Printf("Background consolidation shutting down")
			return
		}
	}
}
