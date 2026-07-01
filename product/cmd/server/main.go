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
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
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
	machines := newMachines(cfg, log)
	newDriver := driverFactory(cfg, log)

	build := builder.NewSandbox(machines, newDriver, builder.Config{
		SystemPrompt: llm.PlannerSystemPrompt,
		OpencodePort: cfg.OpencodePort,
		AnthropicKey: cfg.AnthropicAPIKey, // opencode needs it inside the sandbox
	})
	assets := newStorage(cfg, log)
	broker := stream.NewBroker(500)
	orch := orchestrator.New(st, intake, planner, gate, build, machines, assets, broker, log)
	orch.RecoverInterrupted(context.Background()) // reap builds left running by a prior run
	sessions := auth.NewSessions(cfg.SessionTTL)

	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
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
	switch {
	case cfg.LLMAPIKey != "":
		log.Info("llm: openai-compatible", "base", cfg.LLMBaseURL, "model", cfg.LLMModel)
		c := llm.NewOpenAICompat(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		return c, c, c
	case cfg.AnthropicAPIKey != "":
		log.Info("llm: anthropic")
		a := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		return a, a, a
	default:
		log.Info("llm: fake (dev)")
		f := llm.NewFake()
		return f, f, f
	}
}

// driverFactory decides how to reach opencode for each build:
//   - OPENCODE_URL set → a fixed opencode server (e.g. an existing one)
//   - else FLY token set → per-sandbox opencode over the Fly private network
//   - else → the dev-mode fake
func driverFactory(cfg config.Config, log *slog.Logger) builder.DriverFactory {
	switch {
	case cfg.OpencodeURL != "":
		log.Info("opencode: fixed http server", "url", cfg.OpencodeURL)
		return func(string) opencode.Driver { return opencode.NewHTTP(cfg.OpencodeURL) }
	case cfg.FlyAPIToken != "":
		log.Info("opencode: per-sandbox http over Fly private network")
		return func(addr string) opencode.Driver {
			if addr == "" {
				return opencode.NewFake()
			}
			return opencode.NewHTTP(addr)
		}
	default:
		log.Info("opencode: fake (dev)")
		return func(string) opencode.Driver { return opencode.NewFake() }
	}
}

func newStorage(cfg config.Config, log *slog.Logger) storage.Store {
	if !cfg.StorageEnabled() {
		log.Info("storage: in-memory (dev)")
		return storage.NewMemory()
	}
	log.Info("storage: s3-compatible", "endpoint", cfg.StorageEndpoint, "bucket", cfg.StorageBucket)
	s, err := storage.NewS3(storage.NewS3Params{
		Endpoint:  cfg.StorageEndpoint,
		AccessKey: cfg.StorageAccessKey,
		SecretKey: cfg.StorageSecretKey,
		Bucket:    cfg.StorageBucket,
		Region:    cfg.StorageRegion,
		UseSSL:    cfg.StorageUseSSL,
	})
	if err != nil {
		log.Error("storage init", "err", err)
		os.Exit(1)
	}
	return s
}

func newMachines(cfg config.Config, log *slog.Logger) fly.Machines {
	if cfg.FlyAPIToken == "" {
		log.Info("fly: fake (dev)")
		return fly.NewFake()
	}
	log.Info("fly: http")
	return fly.NewHTTP(fly.Options{
		Token:        cfg.FlyAPIToken,
		Org:          cfg.FlyOrg,
		DeployToken:  cfg.FlyDeployToken,
		SandboxApp:   cfg.FlySandboxApp,
		SandboxImage: cfg.FlySandboxImage,
	})
}
