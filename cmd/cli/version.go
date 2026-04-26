package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

func versionCmd(ctx context.Context, cmd *cli.Command) error {
	fmt.Println("Stash v0.1.0-alpha")
	return nil
}
