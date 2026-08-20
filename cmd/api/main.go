// Command api runs the HTTP server.
//
// main does four things and nothing else: load config, build the logger, wire the
// server, and translate OS signals into context cancellation. All real logic
// lives in internal/server.
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
	// Load .env for local development. Ignore the error: in a deployed
	// environment there is no .env file and the variables come from the
	// orchestrator, which is not a failure.
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

	log := logger.New(cfg.Observ.LogLevel, cfg.Observ.LogFormat)

	// NotifyContext cancels ctx on SIGINT/SIGTERM, which is what triggers the
	// graceful drain. Kubernetes sends SIGTERM before it kills a pod, so honouring
	// it is the difference between a clean deploy and dropped requests.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
