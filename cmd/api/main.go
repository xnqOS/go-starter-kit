package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/xnqOS/go-starter-kit/kit/logging"
	"starter-kit/internal/app"
	"starter-kit/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (result error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger, closeLogs, err := logging.New(cfg.Log)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	defer func() {
		if result != nil {
			logger.Error("application stopped", "error", result)
		}
		result = errors.Join(result, closeLogs())
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer application.Close()
	return application.Run(ctx)
}
