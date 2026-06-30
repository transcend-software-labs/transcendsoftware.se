// Command server runs Transcend Forge: the public landing page, auth, and
// the customer dashboard, plus the orchestrator that plans, screens and builds
// projects.
//
// With no environment configured it runs fully in dev mode (in-memory store,
// fake planner/gate/builder) so the whole flow works locally. Set DATABASE_URL,
// ANTHROPIC_API_KEY, OPENCODE_URL and FLY_API_TOKEN to switch on real services.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	st, err := newStore(cfg, log)
	if err != nil {
		log.Error("store init", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	intake, planner, gate := newLLM(cfg, log)
	driver := newDriver(cfg, log)
	machines := newMachines(cfg, log)

	build := builder.NewSandbox(driver, machines, llm.PlannerSystemPrompt)
	broker := stream.NewBroker(500)
	orch := orchestrator.New(st, intake, planner, gate, build, broker, log)
	sessions := auth.NewSessions(cfg.SessionTTL)

	srv, err := web.NewServer(cfg, st, sessions, orch, broker, log)
	if err != nil {
		log.Error("server init", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("starting", "addr", cfg.Addr, "dev_mode", cfg.DevMode())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func newStore(cfg config.Config, log *slog.Logger) (store.Store, error) {
	if cfg.DatabaseURL == "" {
		log.Info("store: in-memory (dev)")
		return store.NewMemory(), nil
	}
	log.Info("store: postgres")
	return store.NewPostgres(context.Background(), cfg.DatabaseURL)
}

func newLLM(cfg config.Config, log *slog.Logger) (llm.Intake, llm.Planner, llm.SafetyGate) {
	if cfg.AnthropicAPIKey == "" {
		log.Info("llm: fake (dev)")
		f := llm.NewFake()
		return f, f, f
	}
	log.Info("llm: anthropic")
	a := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	return a, a, a
}

func newDriver(cfg config.Config, log *slog.Logger) opencode.Driver {
	if cfg.OpencodeURL == "" {
		log.Info("opencode: fake (dev)")
		return opencode.NewFake()
	}
	log.Info("opencode: http", "url", cfg.OpencodeURL)
	return opencode.NewHTTP(cfg.OpencodeURL)
}

func newMachines(cfg config.Config, log *slog.Logger) fly.Machines {
	if cfg.FlyAPIToken == "" {
		log.Info("fly: fake (dev)")
		return fly.NewFake()
	}
	log.Info("fly: http")
	return fly.NewHTTP(cfg.FlyAPIToken, cfg.FlySandboxApp, cfg.FlySandboxImage)
}
