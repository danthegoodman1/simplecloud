// Command scagent runs inside a sandbox beside the application.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/danthegoodman1/simplecloud/internal/agent"
)

func main() {
	configPath := flag.String("config", agent.ConfigPath, "path to the agent configuration")
	flag.Parse()

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scagent: "+err.Error())
		os.Exit(2)
	}
	a, err := agent.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scagent: "+err.Error())
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "scagent: "+err.Error())
		os.Exit(1)
	}
}
