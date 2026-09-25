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

	"github.com/krelinga/claude-spool-be/backend/internal/api"
	"github.com/krelinga/claude-spool-be/backend/internal/auth"
	"github.com/krelinga/claude-spool-be/backend/internal/claudecli"
	"github.com/krelinga/claude-spool-be/backend/internal/config"
	"github.com/krelinga/claude-spool-be/backend/internal/event"
	"github.com/krelinga/claude-spool-be/backend/internal/executor"
	"github.com/krelinga/claude-spool-be/backend/internal/store"
	"github.com/krelinga/claude-spool-be/backend/internal/webhook"
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
		fmt.Printf("config ok: %d queues (%v), %d tokens, %d webhook receivers\n",
			len(queues.Queues), queues.Names(), len(cfg.Tokens), len(cfg.Webhooks))
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

	// The broker is the live SSE fan-out; the notifier decides who hears what
	// and writes outbox rows; the sender drains the outbox. The executor only
	// ever writes rows, so a down receiver cannot hold up a job (§3.7).
	broker := event.NewBroker()
	notifier := event.NewNotifier(cfg, queueSet, broker)
	sender := webhook.New(cfg, st, log)

	// One lock in front of every claude subprocess: jobs, the keep-alive, and
	// the interactive re-login all hold it, so nothing races the refresh token
	// (§3.2). This is why the executor is global in the first place.
	lock := claudecli.NewLock()

	exec := executor.New(cfg, st, queueSet, notifier, log,
		executor.WithSenderWake(sender.Wake), executor.WithLock(lock))
	authMgr := auth.New(cfg, st, lock, notifier, log,
		auth.WithSenderWake(sender.Wake), auth.WithExecutorWake(exec.Wake))
	exec.SetAuthObserver(authMgr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := exec.Recover(ctx); err != nil {
		return fmt.Errorf("recover state: %w", err)
	}

	// Leftover work directories from a crashed run must not be inherited: a
	// fresh empty directory per job is an invariant (§3.1).
	pruner := executor.NewPruner(cfg, st, queueSet, log)
	pruner.SweepWorkDirs()

	reloader := executor.NewReloader(*queuesPath, queueSet, current.Store, st, log)
	reloader.OnQueueRemoved(func(queue string, jobs []string) {
		for _, id := range jobs {
			exec.NotifyJob(ctx, id)
		}
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(cfg, st, queueSet, exec, authMgr, notifier, broker, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: transcript reads stream, and a live tail will hold
		// the connection open once SSE lands.
	}

	errs := make(chan error, 6)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "queues", queueSet().Names(),
			"credential_mode", cfg.Claude.CredentialMode, "webhook_receivers", len(cfg.Webhooks))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()
	go func() {
		if err := exec.Run(ctx); err != nil {
			errs <- fmt.Errorf("executor: %w", err)
		}
	}()
	go func() {
		if err := sender.Run(ctx); err != nil {
			errs <- fmt.Errorf("webhook sender: %w", err)
		}
	}()
	go func() {
		if err := authMgr.Run(ctx); err != nil {
			errs <- fmt.Errorf("auth manager: %w", err)
		}
	}()
	go func() {
		if err := reloader.Run(ctx); err != nil {
			errs <- fmt.Errorf("queue reloader: %w", err)
		}
	}()
	go func() {
		if err := pruner.Run(ctx); err != nil {
			errs <- fmt.Errorf("pruner: %w", err)
		}
	}()

	// SIGHUP reloads queues.yaml immediately, for an operator who does not want
	// to wait for the poll.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				log.Info("SIGHUP received; reloading queues")
				reloader.ReloadIfChanged(ctx)
			}
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
