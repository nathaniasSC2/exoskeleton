// exo is a small-context agent harness for a home server.
//
//	exo serve    run everything (the systemd unit)
//	exo budget   measure the fixed token cost of each person's prompt and tools
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/nathaniasSC2/exoskeleton/app"
	"github.com/nathaniasSC2/exoskeleton/config"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	a, err := app.New(cfg)
	if err != nil {
		fatal(err)
	}
	switch cmd {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		err = a.Run(ctx)
	case "budget":
		a.Budget(os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "exo:", err)
	os.Exit(1)
}
