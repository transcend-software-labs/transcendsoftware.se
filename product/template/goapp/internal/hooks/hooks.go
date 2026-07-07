// Package hooks delivers notifications when rows are inserted into any table.
//
// Capture is done in SQL (an AFTER INSERT trigger per hooked table writes the
// new row's rowid to _outbox — see the web package, which owns hook config and
// trigger lifecycle). This package is the delivery half: a Dispatcher polls
// _outbox, loads each new row, and fires every enabled hook on its table via a
// Notifier registered for the hook's type. v1 ships the email notifier; Slack
// and generic webhooks are just more Notifier implementations.
package hooks

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Field is one column of a delivered row (secret-shaped columns are masked).
type Field struct {
	Name  string
	Value string
}

// Event is a rendered row handed to a Notifier.
type Event struct {
	Site    string  // site name, for subjects/bodies
	Table   string  // table the row landed in
	Fields  []Field // the row, column → value (masked where secret)
	ReplyTo string  // an email-ish column value, if any
}

// Notifier delivers one Event to a target (address, URL, …).
type Notifier interface {
	Notify(ctx context.Context, target string, e Event) error
}

// Dispatcher polls _outbox and delivers hooks. Zero registered notifiers → it
// still drains the outbox (so it never grows unbounded) but sends nothing.
type Dispatcher struct {
	db        *sql.DB
	site      string
	notifiers map[string]Notifier
	interval  time.Duration
	log       *slog.Logger
}

// NewDispatcher builds a dispatcher. site is used in notification copy.
func NewDispatcher(db *sql.DB, site string, notifiers map[string]Notifier, log *slog.Logger) *Dispatcher {
	if site == "" {
		site = "your site"
	}
	return &Dispatcher{db: db, site: site, notifiers: notifiers, interval: 3 * time.Second, log: log}
}

// Run polls until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.Drain(ctx); err != nil {
				d.log.Error("hooks: drain", "err", err)
			}
		}
	}
}

// Drain processes all currently-pending outbox entries. Exported so tests can
// deliver synchronously without running the poll loop.
func (d *Dispatcher) Drain(ctx context.Context) error {
	for {
		rows, err := d.db.QueryContext(ctx,
			`SELECT id, table_name, row_id FROM _outbox WHERE processed_at IS NULL ORDER BY id LIMIT 50`)
		if err != nil {
			return err
		}
		type item struct {
			id, rowID int64
			table     string
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.table, &it.rowID); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, it)
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		for _, it := range batch {
			d.deliver(ctx, it.table, it.rowID)
			_, _ = d.db.ExecContext(ctx,
				`UPDATE _outbox SET processed_at = ? WHERE id = ?`, time.Now().Unix(), it.id)
		}
	}
}

// deliver fires every enabled hook for one inserted row.
func (d *Dispatcher) deliver(ctx context.Context, table string, rowID int64) {
	hooks, err := d.enabledHooks(ctx, table)
	if err != nil {
		d.log.Error("hooks: load", "table", table, "err", err)
		return
	}
	if len(hooks) == 0 {
		return
	}
	e, err := d.renderRow(ctx, table, rowID)
	if err != nil {
		d.log.Error("hooks: render row", "table", table, "row", rowID, "err", err)
		return
	}
	for _, h := range hooks {
		status := "ok"
		if n := d.notifiers[h.htype]; n != nil {
			if err := d.notifyWithRetry(ctx, n, h.target, e); err != nil {
				status = "error: " + err.Error()
				d.log.Error("hooks: notify", "type", h.htype, "err", err)
			}
		} else {
			status = "error: no " + h.htype + " sender configured"
		}
		_, _ = d.db.ExecContext(ctx,
			`UPDATE _hooks SET last_status = ?, last_at = ? WHERE id = ?`,
			truncate(status, 300), time.Now().Unix(), h.id)
	}
}

func (d *Dispatcher) notifyWithRetry(ctx context.Context, n Notifier, target string, e Event) error {
	err := n.Notify(ctx, target, e)
	if err == nil {
		return nil
	}
	select { // one retry after a short pause
	case <-ctx.Done():
		return err
	case <-time.After(2 * time.Second):
	}
	return n.Notify(ctx, target, e)
}

type hookRow struct {
	id     string
	htype  string
	target string
}

func (d *Dispatcher) enabledHooks(ctx context.Context, table string) ([]hookRow, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, type, target FROM _hooks WHERE table_name = ? AND enabled = 1`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hookRow
	for rows.Next() {
		var h hookRow
		if err := rows.Scan(&h.id, &h.htype, &h.target); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// renderRow loads one row and formats it into an Event, masking secret columns
// and picking an email-ish column as Reply-To. Column names come from the
// result set, so no schema-specific code is needed.
func (d *Dispatcher) renderRow(ctx context.Context, table string, rowID int64) (Event, error) {
	// table is a real table name (a trigger for it exists); still quote it.
	q := `SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" WHERE rowid = ?`
	rows, err := d.db.QueryContext(ctx, q, rowID)
	if err != nil {
		return Event{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return Event{}, err
	}
	if !rows.Next() {
		return Event{}, fmt.Errorf("row %d gone", rowID)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return Event{}, err
	}
	e := Event{Site: d.site, Table: table}
	for i, c := range cols {
		if Masked(c) {
			e.Fields = append(e.Fields, Field{Name: c, Value: "•••••"})
			continue
		}
		v := valueString(vals[i])
		e.Fields = append(e.Fields, Field{Name: c, Value: v})
		if e.ReplyTo == "" && looksLikeEmailColumn(c) && strings.Contains(v, "@") {
			e.ReplyTo = v
		}
	}
	return e, nil
}

func valueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func looksLikeEmailColumn(name string) bool {
	return strings.Contains(strings.ToLower(name), "email") || strings.EqualFold(name, "e_mail")
}

// Masked reports whether a column's values must never be shown/sent. Kept in
// sync with the site admin's masking.
func Masked(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"password", "hash", "token", "secret"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
