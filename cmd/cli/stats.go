package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

func statsCmd(ctx context.Context, cmd *cli.Command) error {
	bc := getBootstrap(cmd)
	stats, err := bc.Brain.GetStats(ctx)
	if err != nil {
		return err
	}

	fmt.Println("--- Stash Statistics ---")
	fmt.Printf("Namespaces: %d\n", stats["namespaces"])
	fmt.Printf("Episodes:   %d\n", stats["episodes"])
	fmt.Printf("Facts:      %d\n", stats["facts"])
	fmt.Printf("Failures:   %d\n", stats["failures"])

	return nil
}
