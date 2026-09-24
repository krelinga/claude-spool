// Command spool runs the queue service: HTTP API, scheduler, and the single
// Claude executor, in one process.
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/krelinga/claude-spool-be/internal/api"
	"github.com/krelinga/claude-spool-be/internal/config"
	"github.com/krelinga/claude-spool-be/internal/executor"
	"github.com/krelinga/claude-spool-be/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spool:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "/etc/spool/config.yaml", "path to config.yaml")
		queuesPath = flag.String("queues", "/etc/spool/queues.yaml", "path to queues.yaml")
		checkOnly  = flag.Bool("check", false, "validate configuration and exit")
		logLevel   = flag.String("log-level", "info", "debug, info, warn, or error")
	)
	flag.Parse()

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	queues, err := config.LoadQueues(*queuesPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("config ok: %d queues (%v), %d tokens\n",
			len(queues.Queues), queues.Names(), len(cfg.Tokens))
		return nil
	}

	// A login-mode container must never carry a credential override: either one
	// outranks the claude.ai login and silently disables skills and connectors
	// (§3.1). The executor strips them per-subprocess; refusing here as well
	// makes the misconfiguration obvious instead of mysterious.
	if cfg.Claude.CredentialMode == config.ModeLogin {
		for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
			if os.Getenv(name) != "" {
				return fmt.Errorf("%s is set, which would override the claude.ai login and disable "+
					"skills and connectors; unset it or switch credential_mode to long_lived_token", name)
			}
		}
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	// queues is behind an atomic pointer so hot-reload (§3.3) can swap it
	// without restarting the executor. Reload itself is not wired up yet.
	var current atomic.Pointer[config.QueueSet]
	current.Store(queues)
	queueSet := func() *config.QueueSet { return current.Load() }

	exec := executor.New(cfg, st, queueSet, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := exec.Recover(ctx); err != nil {
		return fmt.Errorf("recover state: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, queueSet, exec, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: transcript reads stream, and a live tail will hold
		// the connection open once SSE lands.
	}

	errs := make(chan error, 2)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "queues", queueSet().Names(),
			"credential_mode", cfg.Claude.CredentialMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()
	go func() {
		if err := exec.Run(ctx); err != nil {
			errs <- fmt.Errorf("executor: %w", err)
		}
	}()

	select {
	case err := <-errs:
		stop()
		shutdown(srv, log)
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// The executor stops as soon as the current job ends; a running job is left
	// to be recovered as interrupted rather than killed mid-side-effect.
	shutdown(srv, log)
	if id, queue, ok := exec.Running(); ok {
		log.Warn("still running at shutdown; will be marked interrupted", "job", id, "queue", queue)
	}
	return nil
}

func shutdown(srv *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
