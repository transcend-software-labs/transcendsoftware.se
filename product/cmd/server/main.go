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
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/cloudflare"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/glesys"
	"github.com/transcend-software-labs/rasmus-ai/internal/hostup"
	"github.com/transcend-software-labs/rasmus-ai/internal/imagegen"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/notify"
	"github.com/transcend-software-labs/rasmus-ai/internal/oauth"
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
		SystemPrompt: llm.BuildSystemPrompt, // build-and-deploy prompt for the sandbox agent
		OpencodePort: cfg.OpencodePort,
		AnthropicKey: cfg.AnthropicAPIKey, // opencode uses this if set
		LLMBaseURL:   cfg.LLMBaseURL,      // else an OpenAI-compatible model (Moonshot/Kimi)
		LLMKey:       cfg.LLMAPIKey,
		LLMModel:     cfg.LLMModel,

		BackupBucket:    cfg.BackupBucket, // per-app litestream backups (empty → off)
		BackupEndpoint:  cfg.BackupEndpoint,
		BackupRegion:    cfg.BackupRegion,
		BackupAccessKey: cfg.BackupAccessKey,
		BackupSecretKey: cfg.BackupSecretKey,

		SitesEmailKey:  cfg.SitesEmailKey, // generated sites' notification email (empty → off)
		SitesEmailFrom: cfg.SitesEmailFrom,

		Impeccable: cfg.Impeccable, // design-quality gate (A/B via IMPECCABLE_ENABLED)
	})
	assets := newStorage(cfg, log)
	broker := stream.NewBroker(500)
	orch := orchestrator.New(st, intake, planner, gate, build, machines, assets, broker, newVerifier(cfg, log), log)
	orch.SetNotifications(newNotifier(cfg, log), cfg.AdminEmail, cfg.BaseURL)
	if cfg.TemplateKey != "" {
		log.Info("template: starter app enabled", "key", cfg.TemplateKey)
		orch.SetTemplate(cfg.TemplateKey)
	}
	// The active model wiring, so `fly logs` always shows which experiment is
	// live (see `make model-*`). Never log keys.
	log.Info("llm: model config",
		"impl_model", cfg.LLMModel, "impl_base", cfg.LLMBaseURL,
		"planner_model", cfg.PlannerLLMModel, "planner_base", cfg.PlannerLLMBaseURL)
	orch.SetModels(cfg.LLMModel, cfg.PlannerLLMModel)
	// Per-build model selection from /admin (config.ModelProfile registry).
	orch.SetModelProfiles(cfg)
	orch.RecoverInterrupted(context.Background()) // reap builds left running by a prior run
	// Reap zombie infrastructure hourly: preview apps of failed projects,
	// previews idle past PREVIEW_TTL_DAYS, and leaked sandbox machines.
	// 10-minute cadence: the sweep is cheap, and it bounds how long an orphaned
	// sandbox (agents running, nobody driving) can burn tokens.
	orch.StartReaper(context.Background(), 10*time.Minute, cfg.PreviewTTL)
	// Branded preview URLs: previews are handed out as <host>.<PREVIEW_DOMAIN>
	// and reverse-proxied by the web layer, hiding the internal fly.dev URLs.
	if cfg.PreviewDomain != "" {
		log.Info("previews: branded domain enabled", "domain", cfg.PreviewDomain)
		orch.SetPreviewDomain(cfg.PreviewDomain)
		// Self-provision the wildcard cert on this app (idempotent — Fly returns
		// the existing cert on repeat) and log the DNS records the owner must set
		// at the DNS host. Best-effort: until DNS + cert are live, each build's
		// branded verify falls back to the direct URL.
		if cfg.FlyAPIToken != "" && cfg.FlyAppName != "" {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				req, err := machines.AddCertificate(ctx, cfg.FlyAppName, "*."+cfg.PreviewDomain)
				if err != nil {
					log.Warn("previews: wildcard cert provisioning failed", "err", err)
					return
				}
				for _, rec := range req.Records {
					log.Info("previews: required DNS record", "type", rec.Type, "name", rec.Name, "value", rec.Value)
				}
			}()
		}
		// Rewrite existing fly.dev preview URLs to the branded host, but only
		// once the wildcard DNS actually resolves (canary probe; retried on the
		// next boot until the owner has set the records).
		go func() {
			if _, err := net.LookupHost("preview-canary." + cfg.PreviewDomain); err != nil {
				log.Info("previews: wildcard DNS not live yet — backfill skipped", "err", err)
				return
			}
			orch.BackfillPreviewHosts(context.Background())
		}()
	}
	sessions := auth.NewSessions(st, cfg.SessionTTL)

	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		log.Error("server init", "err", err)
		os.Exit(1)
	}
	reg := oauth.NewRegistry(
		oauth.Google(cfg.GoogleClientID, cfg.GoogleClientSecret),
		oauth.LinkedIn(cfg.LinkedInClientID, cfg.LinkedInClientSecret),
	)
	if len(reg.Enabled()) > 0 {
		log.Info("auth: social login enabled", "providers", len(reg.Enabled()))
	}
	srv.SetAuth(reg, newNotifier(cfg, log))
	if cfg.ImageGenEnabled() {
		log.Info("imagegen: enabled", "model", cfg.ImageGenModel, "base", cfg.ImageGenBaseURL)
		srv.SetImageGen(imagegen.New(cfg.ImageGenBaseURL, cfg.ImageGenAPIKey, cfg.ImageGenModel))
	}
	var bill *billing.Client
	if cfg.StripeEnabled() {
		log.Info("billing: stripe enabled", "price", cfg.StripePriceID)
		bill = billing.New("https://api.stripe.com", cfg.StripeSecretKey)
		srv.SetBilling(bill)
	}
	// Domain registrar: GleSYS takes precedence over Hostup and Cloudflare when
	// configured — all three implement the same orchestrator interface so the
	// rest is identical. bill may be nil (Stripe off) — then purchased domains
	// are comped and the operator is alerted. Pass an untyped nil so the
	// interface is truly nil.
	var domainReg orchestrator.DomainRegistrar
	var domainCap float64
	switch {
	case cfg.GlesysEnabled():
		log.Info("domains: glesys enabled", "buy", cfg.DomainBuyEnabled(), "registrant_ok", cfg.GlesysRegistrant.Complete())
		if !cfg.GlesysRegistrant.Complete() {
			log.Warn("domains: glesys registrant incomplete — registrations will fail until GLESYS_REGISTRANT_* (org number, address, zip, city, phone) is set")
		}
		domainReg = glesys.New(cfg.GlesysProjectID, cfg.GlesysAPIKey, glesys.Registrant(cfg.GlesysRegistrant))
		domainCap = cfg.MaxDomainSEK
	case cfg.HostupEnabled():
		log.Info("domains: hostup enabled", "buy", cfg.DomainBuyEnabled(), "base", cfg.HostupAPIURL)
		domainReg = hostup.New(cfg.HostupAPIURL, cfg.HostupAPIToken, cfg.HostupPaymentMethod)
		domainCap = cfg.MaxDomainSEK
	case cfg.CloudflareEnabled():
		log.Info("domains: cloudflare enabled", "buy", cfg.DomainBuyEnabled())
		domainReg = cloudflare.New("https://api.cloudflare.com/client/v4", cfg.CloudflareAPIToken, cfg.CloudflareAccountID)
		domainCap = cfg.MaxDomainUSD
	}
	if domainReg != nil {
		if bill != nil {
			orch.SetDomains(domainReg, bill, domainCap)
		} else {
			orch.SetDomains(domainReg, nil, domainCap)
		}
		orch.StartDomainPoller(context.Background(), 3*time.Minute)
		// Re-bill yearly domain auto-renewals to the customer (no-op without a
		// biller). Slow cadence — renewals happen once a year per domain.
		orch.StartDomainRenewalPoller(context.Background(), 24*time.Hour)
	}
	// Forge Pro change model: the monthly change allowance + flat overage price.
	// Always active (core to the paid product). bill may be nil (Stripe off) —
	// then overage is comped; pass untyped nil so the interface is truly nil.
	log.Info("changes: policy", "per_month", cfg.ChangesPerMonth, "overage_ore", cfg.OverageOre)
	if bill != nil {
		orch.SetChangePolicy(bill, cfg.ChangesPerMonth, cfg.OverageOre)
	} else {
		orch.SetChangePolicy(nil, cfg.ChangesPerMonth, cfg.OverageOre)
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
	// Base client drives intake + safety-gate (and the plan step unless a
	// dedicated planner is configured below).
	var intake llm.Intake
	var planner llm.Planner
	var gate llm.SafetyGate
	switch {
	case cfg.LLMAPIKey != "":
		log.Info("llm: openai-compatible", "base", cfg.LLMBaseURL, "model", cfg.LLMModel)
		c := llm.NewOpenAICompat(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		intake, planner, gate = c, c, c
	case cfg.AnthropicAPIKey != "":
		log.Info("llm: anthropic")
		a := llm.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel, "")
		intake, planner, gate = a, a, a
	default:
		log.Info("llm: fake (dev)")
		f := llm.NewFake()
		intake, planner, gate = f, f, f
	}
	// Optionally run the PLAN step on a dedicated model (e.g. GLM 5.2 via
	// OpenCode Zen), leaving intake/gate/impl unchanged.
	if cfg.PlannerLLMAPIKey != "" {
		log.Info("llm: dedicated planner", "base", cfg.PlannerLLMBaseURL, "model", cfg.PlannerLLMModel)
		planner = llm.NewOpenAICompat(cfg.PlannerLLMBaseURL, cfg.PlannerLLMAPIKey, cfg.PlannerLLMModel)
	}
	return intake, planner, gate
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

// newNotifier picks the email backend: Resend when RESEND_API_KEY is set,
// otherwise a log-only notifier (dev) so the flow works without a provider.
func newNotifier(cfg config.Config, log *slog.Logger) notify.Notifier {
	if cfg.ResendAPIKey != "" {
		log.Info("notify: resend", "from", cfg.EmailFrom, "reply_to", cfg.EmailReplyTo)
		return notify.NewResend(cfg.ResendAPIKey, cfg.EmailFrom, cfg.EmailReplyTo)
	}
	log.Info("notify: log-only (no RESEND_API_KEY)")
	return notify.Log{Logger: log}
}

// newVerifier picks the preview smoke check: a real HTTP probe when builds are
// real (Fly configured), a no-op in dev mode where fake preview URLs don't exist.
func newVerifier(cfg config.Config, log *slog.Logger) orchestrator.Verifier {
	if cfg.FlyAPIToken == "" {
		log.Info("verifier: noop (dev)")
		return orchestrator.NoopVerifier{}
	}
	log.Info("verifier: http")
	return orchestrator.HTTPVerifier{}
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
		Logger:       log,
	})
}
