// Command aco is the AI Coding Orchestrator CLI.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/0xyu/aco/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], cli.OSSystem())
	stop()
	os.Exit(code)
}
