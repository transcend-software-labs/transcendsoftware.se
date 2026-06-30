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

	// Execution plane (empty → fake driver/machines):
	OpencodeURL     string // fixed opencode server base URL (overrides per-machine)
	OpencodePort    int    // port opencode listens on inside each sandbox machine
	FlyAPIToken     string // Fly API token
	FlySandboxApp   string // Fly app the sandbox machines run under
	FlySandboxImage string // OCI image with opencode + toolchains
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
		OpencodeURL:     os.Getenv("OPENCODE_URL"),
		OpencodePort:    4096,
		FlyAPIToken:     os.Getenv("FLY_API_TOKEN"),
		FlySandboxApp:   os.Getenv("FLY_SANDBOX_APP"),
		FlySandboxImage: os.Getenv("FLY_SANDBOX_IMAGE"),
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
