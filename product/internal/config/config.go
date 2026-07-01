// Package config loads runtime configuration from the environment.
//
// With no secrets set, the app runs in dev mode: in-memory store and fake
// planner/gate/builder, so the whole flow works locally without a database,
// an API key, opencode, or Fly.
package config

import (
	"os"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	Addr         string        // listen address, e.g. ":8080"
	BaseURL      string        // public base URL, e.g. https://app.transcendsoftware.se
	SessionTTL   time.Duration // session lifetime
	SecureCookie bool          // mark the session cookie Secure (set true behind HTTPS)

	DatabaseURL string // Postgres DSN; empty → in-memory store

	AdminEmail string // email that may access the operator/admin views

	AnthropicAPIKey string // empty → fake planner/gate
	AnthropicModel  string // empty → llm.DefaultModel

	// OpenAI-compatible LLM (e.g. Moonshot/Kimi) for intake/plan/gate. Takes
	// precedence over Anthropic when LLMAPIKey is set.
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

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
}

// StorageEnabled reports whether a real S3-compatible backend is configured.
func (c Config) StorageEnabled() bool {
	return c.StorageEndpoint != "" && c.StorageAccessKey != "" && c.StorageSecretKey != ""
}

// Load reads configuration from the environment, applying dev-friendly defaults.
func Load() Config {
	return Config{
		Addr:            listenAddr(),
		BaseURL:         envOr("BASE_URL", "http://localhost:8080"),
		SessionTTL:      30 * 24 * time.Hour,
		SecureCookie:    os.Getenv("SECURE_COOKIE") == "true",
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		AdminEmail:      os.Getenv("ADMIN_EMAIL"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:  os.Getenv("ANTHROPIC_MODEL"),

		LLMBaseURL:      envOr("LLM_BASE_URL", "https://api.moonshot.ai/v1"),
		LLMAPIKey:       os.Getenv("LLM_API_KEY"),
		LLMModel:        envOr("LLM_MODEL", "kimi-k2.7-code"),
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
	}
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
