// Command native-agent-api exposes native coding-agent CLIs over HTTP.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/emreuyguc/native-agent-api/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.OSEnv())
	stop()
	os.Exit(code)
}
