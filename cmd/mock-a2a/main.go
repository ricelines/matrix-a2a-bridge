package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"matrix-a2a-bridge/internal/a2a"
)

func main() {
	listenAddr := flag.String("listen", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	server := &http.Server{
		Addr:    *listenAddr,
		Handler: a2a.NewMockHTTPHandler(),
	}

	logger.Info("starting mock A2A server", "addr", *listenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("mock A2A server stopped with error", "err", err)
		os.Exit(1)
	}
}
