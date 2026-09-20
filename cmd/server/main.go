// Command server wires the configuration, storage engine, service and HTTP
// layers together and runs the database with graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sras/internal/api"
	"sras/internal/config"
	"sras/internal/service"
	"sras/internal/storage"
)

func main() {
	cfg := config.Default()
	flag.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	engine, err := storage.Open(storage.Options{
		Dir:          cfg.DataDir,
		MaxFileSize:  cfg.MaxFileSize,
		Fsync:        storage.FsyncPolicy(cfg.Fsync),
		SyncInterval: cfg.SyncInterval,
		Logger:       log,
	})
	if err != nil {
		log.Error("open storage", "err", err)
		os.Exit(1)
	}
	defer engine.Close()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewServer(service.NewDocuments(engine, log), log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", cfg.Addr, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	log.Info("stopped")
}
