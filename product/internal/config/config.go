// Package config loads runtime configuration from the environment.
//
// With no secrets set, the app runs in dev mode: in-memory store and fake
// planner/gate/builder, so the whole flow works locally without a database,
// an API key, opencode, or Fly.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	Addr         string        // listen address, e.g. ":8080"
	BaseURL      string        // public base URL, e.g. https://forge.transcendsoftware.se
	SessionTTL   time.Duration // session lifetime
	SecureCookie bool          // mark the session cookie Secure (set true behind HTTPS)

	DatabaseURL string // Postgres DSN; empty → in-memory store

	AdminEmail string // email that may access the operator/admin views + gets notices

	// Email (empty key → log-only notifier; no mail sent).
	ResendAPIKey string // Resend API key
	EmailFrom    string // verified sender, e.g. "Transcend Forge <hello@forge.transcendsoftware.se>"
	EmailReplyTo string // monitored inbox for replies (sending subdomain can't receive mail)

	// Social login (empty client id/secret → that provider is hidden).
	GoogleClientID       string
	GoogleClientSecret   string
	LinkedInClientID     string
	LinkedInClientSecret string

	// MagicLinkEnabled advertises passwordless email login on the auth pages.
	// Off until email can actually be delivered to arbitrary customers (a
	// verified sender domain) — otherwise the page offers a method that
	// silently fails. Default on; set MAGIC_LINK_ENABLED=false to hide it.
	MagicLinkEnabled bool

	// Quotas — every build spends real money (sandbox machine + LLM tokens).
	MaxProjectsPerDay   int // per user, rolling 24h (default 3)
	MaxConcurrentBuilds int // across all users (default 3)

	// Forge Pro change model: a subscriber gets ChangesPerMonth included changes
	// (fixes/tweaks) that renew monthly; each change beyond that adds a flat
	// OverageOre line to their next Stripe invoice. Expressed in "changes" — the
	// customer never sees AI/token cost.
	ChangesPerMonth int // included changes per month (default 3)
	OverageOre      int // flat price per extra change, in öre (default 4900 = 49 kr)

	// PreviewTTL is how long an untouched preview app stays up before the
	// reaper destroys it and marks the project expired (default 14 days).
	PreviewTTL time.Duration

	// PreviewDomain serves previews under our own domain: customers get
	// https://<slug>-<id>.<PreviewDomain> (reverse-proxied by the Forge app,
	// wildcard cert) instead of the internal fly.dev URL. Empty = off.
	// Needs a "*.<PreviewDomain>" CNAME to the Forge app + the wildcard cert
	// (self-provisioned at startup; see cmd/server/main.go).
	PreviewDomain string

	// FlyAppName is the Forge app's own Fly app name — injected by the Fly
	// runtime as FLY_APP_NAME; used to self-provision the wildcard preview
	// cert on this app. Empty outside Fly.
	FlyAppName string

	// SandboxCostPerHour is the (estimated) $/hour a build sandbox machine
	// costs, used only to show rough per-build cost in /admin. Tune to your
	// machine size; default is a shared-cpu-2x/2GB ballpark.
	SandboxCostPerHour float64

	// TemplateKey is the object-storage key of the starter-app tarball that
	// seeds first builds (empty → greenfield builds).
	TemplateKey string

	AnthropicAPIKey string // empty → fake planner/gate
	AnthropicModel  string // empty → llm.DefaultModel

	// OpenAI-compatible LLM (e.g. Moonshot/Kimi) for intake/plan/gate. Takes
	// precedence over Anthropic when LLMAPIKey is set.
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

	// Optional dedicated model for the PLAN step only (intake + gate keep the
	// LLM_* client above). Lets planning run on a stronger/cheaper model than
	// implementation — e.g. GLM 5.2 via the OpenCode Zen gateway
	// (https://opencode.ai/zen/go/v1). Empty PlannerLLMAPIKey → plan uses the
	// shared LLM_* client, i.e. current behavior.
	PlannerLLMBaseURL string
	PlannerLLMAPIKey  string
	PlannerLLMModel   string

	// Per-build model profiles (see models.go): the operator picks a planner +
	// implementation model per build from /admin. ZenAPIKey/ZenBaseURL back the
	// OpenAI-compatible profiles (Kimi/GLM/Grok/MiniMax/DeepSeek via the OpenCode
	// Zen gateway); MoonshotAPIKey backs the first-party Kimi profiles (same
	// models, Moonshot's own api.moonshot.ai); the anthropic profiles use
	// AnthropicAPIKey. Default* is the combo used when a project hasn't
	// overridden it (reproduces current wiring).
	ZenAPIKey             string
	ZenBaseURL            string
	MoonshotAPIKey        string
	DefaultPlannerProfile string
	DefaultImplProfile    string
	DefaultReviewProfile  string

	// Image generation for content slots ("Generate with AI"). Defaults to
	// OpenAI gpt-image-2 using OPENAI_API_KEY. Disabled when no key is set.
	ImageGenBaseURL       string
	ImageGenAPIKey        string
	ImageGenModel         string
	ImageGenMaxPerProject int // AI generations allowed per project (each is a paid call; default 20)

	// Stripe subscription paywall (all three required to enable — see
	// StripeEnabled). Absent → no payment UI, manual "Mark paid" only.
	StripeSecretKey     string // sk_test_… / sk_live_…
	StripePriceID       string // price_… of the recurring base plan (SEK/month)
	StripeWebhookSecret string // whsec_… to verify webhook signatures

	// name.com as the domain registrar (the single provider — GleSYS, Hostup
	// and Cloudflare were scrapped 2026-07-18 after GleSYS blocked registration
	// account-side). Auth is Basic username + API token; the sandbox
	// (api.dev.name.com) uses the "-test" suffixed username with its own token
	// and simulates registrations. NameComAPIURL defaults by environment:
	// prod URL normally, the sandbox in dev mode; NAME_DOT_COM_API_URL pins it.
	// A purchased domain's 1-year cost is billed once to the customer's next
	// Stripe invoice (clamped to MaxDomainSEK). name.com prices are USD —
	// converted to SEK at SekPerUSD so the whole pipeline stays in SEK.
	NameComUsername string
	NameComAPIKey   string
	NameComAPIURL   string
	DomainMarkupPct float64 // Forge's margin on domain prices, percent (default 10)

	MaxDomainSEK float64 // buy cap, in SEK: max we offer AND max we bill (default 300)

	// Grok Build (xAI's coding-agent CLI) as an alternative sandbox build agent,
	// selectable per project from /admin. Headless runs need a real xAI API key
	// (api.x.ai — the OpenCode Zen gateway key does NOT work here). Absent key →
	// the option is hidden and every build uses opencode.
	XAIAPIKey      string
	GrokBuildModel string // pins -m for grok headless; empty = the CLI's own default model

	// Execution plane (empty → fake driver/machines):
	OpencodeURL     string // fixed opencode server base URL (overrides per-machine)
	OpencodePort    int    // port opencode listens on inside each sandbox machine
	FlyAPIToken     string // Fly API token (trusted side only)
	FlyOrg          string // Fly org slug for per-customer app creation
	FlyDeployToken  string // deploy-scoped token injected into the sandbox for `fly deploy`
	FlySandboxApp   string // Fly app the sandbox machines run under
	FlySandboxImage string // OCI image with opencode + toolchains

	// Object storage for uploaded assets (empty endpoint → in-memory dev store).
	// MinIO locally, Fly Tigris in production — both S3-compatible.
	StorageEndpoint  string // host:port (MinIO) or host (Tigris)
	StorageAccessKey string
	StorageSecretKey string
	StorageBucket    string
	StorageRegion    string
	StorageUseSSL    bool

	// Backup* is a separate object-storage bucket for per-app SQLite backups
	// (litestream). Injected into each generated site as app secrets; empty
	// bucket → sites deploy without continuous backup. Kept distinct from the
	// asset bucket so a site's backup credential can't reach customer assets.
	BackupBucket    string
	BackupEndpoint  string
	BackupRegion    string
	BackupAccessKey string
	BackupSecretKey string

	// Generated sites' notification email (empty key → sites deploy without
	// email hooks). One sending-only, domain-restricted key is shared across
	// sites for now; SitesEmailFrom is the sender address on the verified
	// forge domain (the site's own name becomes the display name).
	SitesEmailKey  string
	SitesEmailFrom string

	// Impeccable turns on the design-quality gate: the build agent runs the
	// impeccable detector on its UI before deploying and fixes findings. A
	// switch so we can A/B its build-time cost vs. quality. (The design
	// principles in the template's AGENTS.md apply either way.)
	Impeccable bool
}

// StorageEnabled reports whether a real S3-compatible backend is configured.
func (c Config) StorageEnabled() bool {
	return c.StorageEndpoint != "" && c.StorageAccessKey != "" && c.StorageSecretKey != ""
}

// Load reads configuration from the environment, applying dev-friendly defaults.
func Load() Config {
	c := Config{
		Addr:                 listenAddr(),
		BaseURL:              envOr("BASE_URL", "http://localhost:8080"),
		SessionTTL:           30 * 24 * time.Hour,
		SecureCookie:         os.Getenv("SECURE_COOKIE") == "true",
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		AdminEmail:           os.Getenv("ADMIN_EMAIL"),
		ResendAPIKey:         os.Getenv("RESEND_API_KEY"),
		EmailFrom:            envOr("EMAIL_FROM", "Transcend Forge <onboarding@resend.dev>"),
		EmailReplyTo:         os.Getenv("EMAIL_REPLY_TO"),
		GoogleClientID:       os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:   os.Getenv("GOOGLE_CLIENT_SECRET"),
		LinkedInClientID:     os.Getenv("LINKEDIN_CLIENT_ID"),
		LinkedInClientSecret: os.Getenv("LINKEDIN_CLIENT_SECRET"),
		MagicLinkEnabled:     os.Getenv("MAGIC_LINK_ENABLED") != "false",

		MaxProjectsPerDay:   envIntOr("MAX_PROJECTS_PER_DAY", 3),
		MaxConcurrentBuilds: envIntOr("MAX_CONCURRENT_BUILDS", 3),
		ChangesPerMonth:     envIntOr("FORGE_CHANGES_PER_MONTH", 3),
		OverageOre:          envIntOr("FORGE_OVERAGE_SEK", 49) * 100,
		PreviewTTL:          time.Duration(envIntOr("PREVIEW_TTL_DAYS", 14)) * 24 * time.Hour,
		PreviewDomain:       strings.TrimSuffix(strings.ToLower(os.Getenv("PREVIEW_DOMAIN")), "."),
		FlyAppName:          os.Getenv("FLY_APP_NAME"),
		SandboxCostPerHour:  envFloatOr("SANDBOX_COST_PER_HOUR", 0.02),
		TemplateKey:         os.Getenv("TEMPLATE_KEY"),

		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:  os.Getenv("ANTHROPIC_MODEL"),

		LLMBaseURL: envOr("LLM_BASE_URL", "https://api.moonshot.ai/v1"),
		LLMAPIKey:  os.Getenv("LLM_API_KEY"),
		LLMModel:   envOr("LLM_MODEL", "kimi-k2.7-code"),

		PlannerLLMBaseURL: envOr("PLANNER_LLM_BASE_URL", "https://opencode.ai/zen/go/v1"),
		PlannerLLMAPIKey:  os.Getenv("PLANNER_LLM_API_KEY"),
		PlannerLLMModel:   envOr("PLANNER_LLM_MODEL", "glm-5.2"),

		ImageGenBaseURL:       envOr("IMAGEGEN_BASE_URL", "https://api.openai.com/v1"),
		ImageGenAPIKey:        os.Getenv("IMAGEGEN_API_KEY"),
		ImageGenModel:         envOr("IMAGEGEN_MODEL", "gpt-image-2"),
		ImageGenMaxPerProject: envIntOr("IMAGEGEN_MAX_PER_PROJECT", 20),

		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripePriceID:       os.Getenv("STRIPE_PRICE_ID"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),

		NameComUsername: os.Getenv("NAME_DOT_COM_USERNAME"),
		NameComAPIKey:   os.Getenv("NAME_DOT_COM_API_KEY"),
		DomainMarkupPct: envFloatOr("DOMAIN_MARKUP_PCT", 10),

		MaxDomainSEK: envFloatOr("MAX_DOMAIN_SEK", 300),

		XAIAPIKey:      os.Getenv("XAI_API_KEY"),
		GrokBuildModel: os.Getenv("GROK_BUILD_MODEL"),

		OpencodeURL:     os.Getenv("OPENCODE_URL"),
		OpencodePort:    4096,
		FlyAPIToken:     os.Getenv("FLY_API_TOKEN"),
		FlyOrg:          envOr("FLY_ORG", "personal"),
		FlyDeployToken:  os.Getenv("FLY_DEPLOY_TOKEN"),
		FlySandboxApp:   os.Getenv("FLY_SANDBOX_APP"),
		FlySandboxImage: os.Getenv("FLY_SANDBOX_IMAGE"),

		StorageEndpoint:  os.Getenv("STORAGE_ENDPOINT"),
		StorageAccessKey: os.Getenv("STORAGE_ACCESS_KEY"),
		StorageSecretKey: os.Getenv("STORAGE_SECRET_KEY"),
		StorageBucket:    envOr("STORAGE_BUCKET", "forge-assets"),
		StorageRegion:    envOr("STORAGE_REGION", "auto"),
		StorageUseSSL:    os.Getenv("STORAGE_USE_SSL") == "true",

		BackupBucket:    os.Getenv("BACKUP_BUCKET"),
		BackupEndpoint:  os.Getenv("BACKUP_ENDPOINT"),
		BackupRegion:    envOr("BACKUP_REGION", "auto"),
		BackupAccessKey: os.Getenv("BACKUP_ACCESS_KEY"),
		BackupSecretKey: os.Getenv("BACKUP_SECRET_KEY"),

		SitesEmailKey:  os.Getenv("SITES_EMAIL_KEY"),
		SitesEmailFrom: os.Getenv("SITES_EMAIL_FROM"),

		Impeccable: os.Getenv("IMPECCABLE_ENABLED") == "true",
	}

	// OPENCODE_GO_API_KEY: a single OpenCode Zen key that backs the whole
	// pipeline (impl+intake+gate on LLMModel, plan on PlannerLLMModel). When set
	// it is the DEFAULT wiring, not a lock: base URLs default to the Zen Go
	// gateway and both clients use this key, but an explicitly set LLM_BASE_URL /
	// PLANNER_LLM_BASE_URL / *_API_KEY env still wins. That makes model
	// experiments env-only — `make model-go / model-zen / model-openai` pick the
	// provider and models per role; `make model-default` unsets everything back
	// to kimi+glm on zen/go/v1.
	if zen := os.Getenv("OPENCODE_GO_API_KEY"); zen != "" {
		const zenBase = "https://opencode.ai/zen/go/v1"
		if os.Getenv("LLM_BASE_URL") == "" {
			c.LLMBaseURL = zenBase
		}
		if os.Getenv("LLM_API_KEY") == "" {
			c.LLMAPIKey = zen
		}
		if os.Getenv("PLANNER_LLM_BASE_URL") == "" {
			c.PlannerLLMBaseURL = zenBase
		}
		if os.Getenv("PLANNER_LLM_API_KEY") == "" {
			c.PlannerLLMAPIKey = zen
		}
	}
	// OPENAI_API_KEY backs any role whose base URL points at OpenAI directly
	// (make model-openai). Runs after the Zen default so it wins for those roles.
	if oa := os.Getenv("OPENAI_API_KEY"); oa != "" {
		if strings.Contains(c.LLMBaseURL, "api.openai.com") && os.Getenv("LLM_API_KEY") == "" {
			c.LLMAPIKey = oa
		}
		if strings.Contains(c.PlannerLLMBaseURL, "api.openai.com") && os.Getenv("PLANNER_LLM_API_KEY") == "" {
			c.PlannerLLMAPIKey = oa
		}
	}

	// Image generation defaults to OpenAI on OPENAI_API_KEY.
	if c.ImageGenAPIKey == "" {
		c.ImageGenAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	// Model profiles: the OpenAI-compatible profiles run on the OpenCode Zen Go
	// gateway. Prefer OPENCODE_GO_API_KEY; else reuse the LLM key when it already
	// points at the Zen gateway (the common default wiring).
	c.ZenBaseURL = "https://opencode.ai/zen/go/v1"
	c.ZenAPIKey = os.Getenv("OPENCODE_GO_API_KEY")
	if c.ZenAPIKey == "" && strings.Contains(c.LLMBaseURL, "opencode.ai/zen") {
		c.ZenAPIKey = c.LLMAPIKey
	}
	// Moonshot first-party (the kimi-*-moonshot profiles): same Kimi models as
	// the Go gateway, but direct — lets a build A/B the route, not just the model.
	c.MoonshotAPIKey = os.Getenv("MOONSHOT_API_KEY")
	// Forge's global default models: Grok 4.5 via the Zen gateway for BOTH the
	// plan and the implementation (Rasmus, 2026-07-15). Projects that don't pin
	// an override track this, so changing it here (or via the env vars) moves
	// every non-overridden project on its next build.
	c.DefaultPlannerProfile = envOr("DEFAULT_PLANNER_PROFILE", "grok")
	c.DefaultImplProfile = envOr("DEFAULT_IMPL_PROFILE", "grok")
	// The post-payment code review defaults to the planner's model — same
	// registry, so the /admin picker offers the identical set.
	c.DefaultReviewProfile = envOr("DEFAULT_REVIEW_PROFILE", c.DefaultPlannerProfile)
	// name.com base URL: production normally; the simulation sandbox when the
	// app runs in dev mode (in-memory everything), so local runs never place
	// real orders. NAME_DOT_COM_API_URL pins either explicitly. Set here (after
	// the struct literal) because DevMode derives from other fields.
	defaultNameComURL := "https://api.name.com"
	if c.DevMode() {
		defaultNameComURL = "https://api.dev.name.com"
	}
	c.NameComAPIURL = envOr("NAME_DOT_COM_API_URL", defaultNameComURL)
	return c
}

// ImageGenEnabled reports whether "Generate with AI" is available.
func (c Config) ImageGenEnabled() bool { return c.ImageGenAPIKey != "" }

// StripeEnabled reports whether the subscription paywall is live. All three
// secrets are required: without the webhook secret we could take a payment we
// can never observe, so the feature stays fully off until it's set.
func (c Config) StripeEnabled() bool {
	return c.StripeSecretKey != "" && c.StripePriceID != "" && c.StripeWebhookSecret != ""
}

// NameComEnabled reports whether name.com backs the custom-domain feature
// (attach your own domain, DNS, buy in-app). Invisible in the UI when off.
func (c Config) NameComEnabled() bool {
	return c.NameComUsername != "" && c.NameComAPIKey != ""
}

// DomainsEnabled reports whether a domain registrar is configured.
func (c Config) DomainsEnabled() bool { return c.NameComEnabled() }

// GrokBuildEnabled reports whether the Grok Build agent can be offered as a
// per-project build-agent choice.
func (c Config) GrokBuildEnabled() bool { return c.XAIAPIKey != "" }

// DomainBuyEnabled reports whether customers can buy a domain in-app. Beyond
// registrar access it needs Stripe live, so we can bill the registration cost
// onto the customer's next invoice — we never register a domain we can't charge
// for.
func (c Config) DomainBuyEnabled() bool {
	return c.DomainsEnabled() && c.StripeEnabled()
}

// DevMode reports whether the app is running fully in-memory/fake.
func (c Config) DevMode() bool {
	return c.DatabaseURL == "" && c.AnthropicAPIKey == "" && c.OpencodeURL == "" && c.FlyAPIToken == ""
}

// listenAddr resolves the listen address, preferring ADDR, then PORT (which
// hosting platforms inject), then a default.
func listenAddr() string {
	if a := os.Getenv("ADDR"); a != "" {
		return a
	}
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}
