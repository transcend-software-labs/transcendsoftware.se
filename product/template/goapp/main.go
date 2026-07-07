// Command app is a single-binary web application: server-rendered frontend,
// backend handlers, embedded templates/assets, SQLite persistence, and cookie
// auth. This is the Transcend Forge starter — build agents extend it per the
// conventions in AGENTS.md.
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

	"app/internal/auth"
	"app/internal/db"
	"app/internal/hooks"
	"app/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dataDir := envOr("DATA_DIR", "data")
	database, err := db.Open(dataDir)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	sessions := auth.NewSessions(database, 30*24*time.Hour)

	// Notification hooks: an email sender (Resend-compatible) when EMAIL_API_KEY
	// + EMAIL_FROM are set. A background dispatcher delivers hooks from _outbox.
	notifiers := map[string]hooks.Notifier{}
	if n := hooks.NewEmailNotifier(os.Getenv("EMAIL_API_KEY"), os.Getenv("EMAIL_FROM")); n != nil {
		notifiers["email"] = n
	}
	siteName := envOr("SITE_NAME", "your site")

	// OWNER_EMAIL (set by the Forge orchestrator) reserves the first — owner —
	// account for the customer the site was built for.
	srv := web.New(database, sessions, web.Options{
		SecureCookie: os.Getenv("SECURE_COOKIE") == "true",
		OwnerEmail:   os.Getenv("OWNER_EMAIL"),
		SiteName:     siteName,
		Notifiers:    notifiers,
	}, log)

	dispatcher := hooks.NewDispatcher(database, siteName, notifiers, log)

	httpSrv := &http.Server{
		Addr:              listenAddr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("starting", "addr", httpSrv.Addr, "data_dir", dataDir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deliver notification hooks until shutdown.
	go dispatcher.Run(ctx)

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// listenAddr prefers ADDR, then PORT (injected by hosting platforms), then :8080.
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
