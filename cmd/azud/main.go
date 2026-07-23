package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lemonity-org/azud/internal/cli"
	"github.com/lemonity-org/azud/internal/output"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.ExecuteContext(ctx); err != nil {
		output.Error("%v", err)
		os.Exit(1)
	}
}
