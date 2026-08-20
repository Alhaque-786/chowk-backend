// Command worker runs the asynchronous half of the system: the outbox relay and
// periodic maintenance jobs.
//
// It is a separate binary from the API but shares the same module wiring, so any
// event subscriber a module registers is live here automatically. Running it as
// its own process means a burst of notifications or a large outbox drain cannot
// steal goroutines and database connections from checkout.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/server"
)

func main() {
	_ = godotenv.Load()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(cfg.Observ.LogLevel, cfg.Observ.LogFormat).With("process", "worker")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return srv.RunWorker(ctx)
}
