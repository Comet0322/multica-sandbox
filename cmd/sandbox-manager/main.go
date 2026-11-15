// sandbox-manager runs the sandbox manager: the Multica runtimes of every
// workspace of a bot account, whose tasks run one at a time on pods the
// manager creates through the Kubernetes API, each running the stock Multica
// daemon. See docs/DEPLOYMENT.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	settings, err := loadSettings(os.Getenv)
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = runManager(ctx, log, settings)
	}
	if err != nil {
		log.Error("sandbox manager stopped", "error", err)
		os.Exit(1)
	}
}
