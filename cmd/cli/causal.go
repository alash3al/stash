package main

import (
	"context"
	"fmt"

	"github.com/alash3al/stash/internal/brain"
	"github.com/urfave/cli/v3"
)

func causalListCmd(ctx context.Context, cmd *cli.Command) error {
	namespaces := cmd.StringSlice("namespaces")
	page := brain.Pagination{
		Offset: cmd.Int("offset"),
		Limit:  cmd.Int("limit"),
	}

	bc := getBootstrap(cmd)
	links, err := bc.Brain.ListCausalLinks(ctx, namespaces, page)
	if err != nil {
		return err
	}
	return printJSON(links)
}

func causalCreateCmd(ctx context.Context, cmd *cli.Command) error {
	causeID := cmd.Int("cause-id")
	effectID := cmd.Int("effect-id")
	effectFailureID := cmd.Int("effect-failure-id")

	if causeID == 0 {
		return fmt.Errorf("--cause-id is required")
	}
	if effectID == 0 && effectFailureID == 0 {
		return fmt.Errorf("either --effect-id or --effect-failure-id is required")
	}

	namespace := cmd.String("namespace")
	confidence := cmd.Float("confidence")
	if confidence == 0 {
		confidence = 0.8
	}

	bc := getBootstrap(cmd)
	nsIDs, err := bc.Brain.ResolveNamespaceIDs(ctx, []string{namespace})
	if err != nil {
		return err
	}

	var effFactID, effFailureID *int64
	if effectID != 0 {
		eid := int64(effectID)
		effFactID = &eid
	}
	if effectFailureID != 0 {
		efid := int64(effectFailureID)
		effFailureID = &efid
	}

	link, err := bc.Brain.CreateCausalLink(ctx, nsIDs[0], int64(causeID), effFactID, effFailureID, float32(confidence))
	if err != nil {
		return err
	}
	return printJSON(link)
}

func causalTraceCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("fact ID argument is required")
	}
	var factID int64
	if _, err := fmt.Sscanf(args.First(), "%d", &factID); err != nil {
		return fmt.Errorf("invalid fact ID: %w", err)
	}

	direction := cmd.String("direction")
	maxDepth := cmd.Int("depth")
	isMermaid := cmd.Bool("mermaid")

	bc := getBootstrap(cmd)
	chain, err := bc.Brain.TraceCausalChain(ctx, factID, direction, maxDepth)
	if err != nil {
		return err
	}

	if !isMermaid {
		return printJSON(chain)
	}

	// Mermaid visualization logic
	factIDs := map[int64]bool{factID: true}
	failureIDs := map[int64]bool{}
	for _, link := range chain {
		factIDs[link.CauseFactID] = true
		if link.EffectFactID != nil {
			factIDs[*link.EffectFactID] = true
		}
		if link.EffectFailureID != nil {
			failureIDs[*link.EffectFailureID] = true
		}
	}

	// Fetch contents
	factContents := make(map[int64]string)
	for fid := range factIDs {
		f, err := bc.Brain.GetFact(ctx, fid)
		if err == nil {
			factContents[fid] = f.Content
		} else {
			factContents[fid] = fmt.Sprintf("Fact %d", fid)
		}
	}

	failureContents := make(map[int64]string)
	for fid := range failureIDs {
		f, err := bc.Brain.GetFailure(ctx, fid)
		if err == nil {
			failureContents[fid] = f.Content
		} else {
			failureContents[fid] = fmt.Sprintf("Failure %d", fid)
		}
	}

	truncate := func(s string) string {
		if len(s) > 50 {
			return s[:47] + "..."
		}
		return s
	}

	fmt.Println("graph TD")
	visited := make(map[string]bool)
	for _, link := range chain {
		var effectStr string
		var effectContent string

		if link.EffectFactID != nil {
			effectStr = fmt.Sprintf("F%d", *link.EffectFactID)
			effectContent = factContents[*link.EffectFactID]
		} else if link.EffectFailureID != nil {
			effectStr = fmt.Sprintf("E%d", *link.EffectFailureID)
			effectContent = failureContents[*link.EffectFailureID]
		} else {
			continue
		}

		edge := fmt.Sprintf("F%d --> %s", link.CauseFactID, effectStr)
		if visited[edge] {
			continue
		}
		visited[edge] = true

		fmt.Printf("  F%d[\"%s\"]\n", link.CauseFactID, truncate(factContents[link.CauseFactID]))
		fmt.Printf("  %s[\"%s\"]\n", effectStr, truncate(effectContent))
		fmt.Printf("  %s\n", edge)
	}

	return nil
}

func causalDeleteCmd(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return fmt.Errorf("causal link ID is required")
	}
	var id int64
	if _, err := fmt.Sscanf(args.First(), "%d", &id); err != nil {
		return fmt.Errorf("invalid causal link ID: %w", err)
	}

	bc := getBootstrap(cmd)
	if err := bc.Brain.DeleteCausalLink(ctx, id); err != nil {
		return err
	}
	return printJSON(map[string]string{"message": "Causal link deleted successfully"})
}
