package main

import (
	"context"
	"os"

	"github.com/netforge/netforge/internal/cli"
	"github.com/netforge/netforge/internal/namespace"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/router"
	"github.com/netforge/netforge/internal/service"
)

var (
	version = ""
	commit  = ""
)

func main() {
	if handled, code := namespace.Dispatch(); handled {
		os.Exit(code)
	}
	if handled, code := router.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := service.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := probe.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	signals := cli.NotifySignals(ctx)
	defer signals.Stop()

	app := &cli.App{
		Env: cli.Env{
			Name:    cli.ProgramName,
			Version: version,
			Commit:  commit,
			Args:    args,
			Stdin:   os.Stdin,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			Getenv:  os.Getenv,
		},
		Version: version,
		Commit:  commit,
	}

	return signals.ExitCode(app.Run(signals.Context()))
}
