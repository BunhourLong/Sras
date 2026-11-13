// Command server wires the configuration, storage engine, service and HTTP
// layers together and runs the database with graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	cfg, err := config.Load(os.Args[1:], os.LookupEnv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0) // usage already printed by Load's FlagSet
		}
		fmt.Fprintf(os.Stderr, "sras: %v\n", err)
		os.Exit(2)
	}

	logOpts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var log *slog.Logger
	if cfg.LogFormat == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stdout, logOpts))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stdout, logOpts))
	}

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
	defer func() { _ = engine.Close() }()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewServer(service.NewDocuments(engine, log), log, cfg.RequestTimeout),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout covers the whole response; keep it above the api
		// request timeout.
		WriteTimeout: cfg.RequestTimeout + 15*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening",
			"addr", cfg.Addr,
			"data", cfg.DataDir,
			"fsync", cfg.Fsync,
			"max_file_size", cfg.MaxFileSize,
			"sync_interval", cfg.SyncInterval,
			"request_timeout", cfg.RequestTimeout,
			"log_format", cfg.LogFormat,
			"log_level", cfg.LogLevel)
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
