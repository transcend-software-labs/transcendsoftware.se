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

	// PreviewTTL is how long an untouched preview app stays up before the
	// reaper destroys it and marks the project expired (default 14 days).
	PreviewTTL time.Duration

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

	// Design critic: a vision-capable model that reviews the deployed site's
	// screenshots after preview_ready and can trigger one internal polish pass.
	// Defaults to the impl LLM wiring; disable with DESIGN_CRITIC=off.
	CriticLLMBaseURL string
	CriticLLMAPIKey  string
	CriticLLMModel   string
	CriticEnabled    bool
	CriticAutoPolish bool

	// Image generation for content slots ("Generate with AI"). Defaults to
	// OpenAI gpt-image-2 using OPENAI_API_KEY. Disabled when no key is set.
	ImageGenBaseURL       string
	ImageGenAPIKey        string
	ImageGenModel         string
	ImageGenMaxPerProject int // AI generations allowed per project (each is a paid call; default 20)

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
		PreviewTTL:          time.Duration(envIntOr("PREVIEW_TTL_DAYS", 14)) * 24 * time.Hour,
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

		CriticLLMBaseURL: os.Getenv("CRITIC_LLM_BASE_URL"),
		CriticLLMAPIKey:  os.Getenv("CRITIC_LLM_API_KEY"),
		CriticLLMModel:   os.Getenv("CRITIC_LLM_MODEL"),
		CriticEnabled:    os.Getenv("DESIGN_CRITIC") != "off",
		CriticAutoPolish: os.Getenv("CRITIC_AUTOPOLISH") != "off",

		ImageGenBaseURL:       envOr("IMAGEGEN_BASE_URL", "https://api.openai.com/v1"),
		ImageGenAPIKey:        os.Getenv("IMAGEGEN_API_KEY"),
		ImageGenModel:         envOr("IMAGEGEN_MODEL", "gpt-image-2"),
		ImageGenMaxPerProject: envIntOr("IMAGEGEN_MAX_PER_PROJECT", 20),
		OpencodeURL:           os.Getenv("OPENCODE_URL"),
		OpencodePort:          4096,
		FlyAPIToken:           os.Getenv("FLY_API_TOKEN"),
		FlyOrg:                envOr("FLY_ORG", "personal"),
		FlyDeployToken:        os.Getenv("FLY_DEPLOY_TOKEN"),
		FlySandboxApp:         os.Getenv("FLY_SANDBOX_APP"),
		FlySandboxImage:       os.Getenv("FLY_SANDBOX_IMAGE"),

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

	// Critic defaults: whatever the impl model resolved to above (Zen/OpenAI
	// included), unless CRITIC_LLM_* points it somewhere else explicitly.
	if c.CriticLLMBaseURL == "" {
		c.CriticLLMBaseURL = c.LLMBaseURL
	}
	if c.CriticLLMModel == "" {
		c.CriticLLMModel = c.LLMModel
	}
	if c.CriticLLMAPIKey == "" {
		if strings.Contains(c.CriticLLMBaseURL, "api.openai.com") {
			c.CriticLLMAPIKey = os.Getenv("OPENAI_API_KEY")
		} else {
			c.CriticLLMAPIKey = c.LLMAPIKey
		}
	}
	// Image generation defaults to OpenAI on OPENAI_API_KEY.
	if c.ImageGenAPIKey == "" {
		c.ImageGenAPIKey = os.Getenv("OPENAI_API_KEY")
	}
	return c
}

// ImageGenEnabled reports whether "Generate with AI" is available.
func (c Config) ImageGenEnabled() bool { return c.ImageGenAPIKey != "" }

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
