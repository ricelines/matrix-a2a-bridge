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

	matrixBot, err := bot.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create bot", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := matrixBot.Run(ctx); err != nil {
		logger.Error("bot stopped with error", "err", err)
		os.Exit(1)
	}
}
