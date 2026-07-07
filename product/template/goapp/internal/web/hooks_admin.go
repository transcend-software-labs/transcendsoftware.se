// Admin-side hook management: the owner turns on notifications for a table, and
// this creates/removes the AFTER INSERT trigger that feeds _outbox. Delivery
// itself lives in internal/hooks (the Dispatcher).
package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"app/internal/auth"
	"app/internal/hooks"
)

// hookInfo is one configured hook, for display.
type hookInfo struct {
	ID         string
	Type       string
	Target     string
	Enabled    bool
	LastStatus string
	EmailReady bool // an email sender is configured
}

// tableHooks lists the hooks on a table, newest first.
func (s *Server) tableHooks(ctx context.Context, table string) ([]hookInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, type, target, enabled, last_status FROM _hooks WHERE table_name = ? ORDER BY created_at DESC`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	_, emailReady := s.notifiers["email"]
	var out []hookInfo
	for rows.Next() {
		var h hookInfo
		var enabled int
		if err := rows.Scan(&h.ID, &h.Type, &h.Target, &enabled, &h.LastStatus); err != nil {
			return nil, err
		}
		h.Enabled = enabled == 1
		h.EmailReady = emailReady
		out = append(out, h)
	}
	return out, rows.Err()
}

// handleHookAdd enables an email hook on a table.
func (s *Server) handleHookAdd(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	table := r.PathValue("table")
	if _, ok := s.adminColumns(r.Context(), table); !ok {
		http.NotFound(w, r)
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if target == "" {
		target = s.ownerEmail
	}
	if !strings.Contains(target, "@") || len(target) > 200 {
		s.redirectTable(w, r, table, "Enter a valid email address to notify.")
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO _hooks (id, table_name, type, target, enabled, created_at)
		 VALUES (?, ?, 'email', ?, 1, ?)
		 ON CONFLICT(table_name, type, target) DO UPDATE SET enabled = 1`,
		auth.NewID(), table, target, time.Now().Unix())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.syncTrigger(r.Context(), table); err != nil {
		s.log.Error("hooks: sync trigger", "table", table, "err", err)
	}
	s.redirectTable(w, r, table, "")
}

// handleHookToggle enables/disables a hook.
func (s *Server) handleHookToggle(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	table, ok := s.flipHook(r.Context(), r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.syncTrigger(r.Context(), table); err != nil {
		s.log.Error("hooks: sync trigger", "table", table, "err", err)
	}
	s.redirectTable(w, r, table, "")
}

// handleHookDelete removes a hook.
func (s *Server) handleHookDelete(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	var table string
	if err := s.db.QueryRowContext(r.Context(), `SELECT table_name FROM _hooks WHERE id = ?`, id).Scan(&table); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM _hooks WHERE id = ?`, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.syncTrigger(r.Context(), table); err != nil {
		s.log.Error("hooks: sync trigger", "table", table, "err", err)
	}
	s.redirectTable(w, r, table, "")
}

// handleHookTest sends a sample notification so the owner can confirm delivery.
func (s *Server) handleHookTest(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	var table, htype, target string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT table_name, type, target FROM _hooks WHERE id = ?`, id).Scan(&table, &htype, &target); err != nil {
		http.NotFound(w, r)
		return
	}
	n := s.notifiers[htype]
	if n == nil {
		s.redirectTable(w, r, table, "Notifications aren't set up for this site yet.")
		return
	}
	e := hooks.Event{Site: s.siteName, Table: table, Fields: []hooks.Field{
		{Name: "test", Value: "This is a test notification — your hook works."},
	}}
	msg := "Test sent to " + target + "."
	if err := n.Notify(r.Context(), target, e); err != nil {
		msg = "Test failed: " + err.Error()
	}
	s.redirectTable(w, r, table, msg)
}

// flipHook toggles enabled and returns the hook's table.
func (s *Server) flipHook(ctx context.Context, id string) (string, bool) {
	var table string
	if err := s.db.QueryRowContext(ctx, `SELECT table_name FROM _hooks WHERE id = ?`, id).Scan(&table); err != nil {
		return "", false
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE _hooks SET enabled = 1 - enabled WHERE id = ?`, id); err != nil {
		return "", false
	}
	return table, true
}

// syncTrigger makes the AFTER INSERT trigger for a table exist iff the table
// has at least one enabled hook. The trigger enqueues each new row into
// _outbox; the dispatcher fans out to the hooks. Idempotent.
func (s *Server) syncTrigger(ctx context.Context, table string) error {
	// Only ever operate on a validated, real table name.
	if _, ok := s.adminColumns(ctx, table); !ok {
		return nil
	}
	var enabled int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM _hooks WHERE table_name = ? AND enabled = 1`, table).Scan(&enabled); err != nil {
		return err
	}
	trigger := quoteIdent("_hook_" + table)
	if enabled == 0 {
		_, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+trigger)
		return err
	}
	stmt := `CREATE TRIGGER IF NOT EXISTS ` + trigger + ` AFTER INSERT ON ` + quoteIdent(table) + `
		BEGIN
		  INSERT INTO _outbox (table_name, row_id, created_at)
		  VALUES (` + sqlLiteral(table) + `, NEW.rowid, CAST(strftime('%s','now') AS INTEGER));
		END`
	_, err := s.db.ExecContext(ctx, stmt)
	return err
}

// redirectTable returns to a table page, optionally carrying a flash message.
func (s *Server) redirectTable(w http.ResponseWriter, r *http.Request, table, flash string) {
	u := "/admin/t/" + table
	if flash != "" {
		u += "?msg=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// sqlLiteral renders a string as a single-quoted SQL literal.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
