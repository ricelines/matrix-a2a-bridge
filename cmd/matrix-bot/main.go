package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"onboarding/internal/bot"
	"onboarding/internal/config"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.FromEnv()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	matrixRuntime, err := bot.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create onboarding-agent Matrix runtime", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := matrixRuntime.Run(ctx); err != nil {
		logger.Error("onboarding-agent Matrix runtime stopped with error", "err", err)
		os.Exit(1)
	}
}
