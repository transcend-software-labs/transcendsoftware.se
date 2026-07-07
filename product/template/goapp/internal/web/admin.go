// The site admin: renders EVERY table in the SQLite database by introspection
// (sqlite_master + PRAGMA table_info), so whatever schema this site has, the
// owner can browse it, export it, and (soon) attach hooks to it — no bespoke
// dashboard code per table.
//
// Safety model: table names from the URL are validated against the introspected
// schema before they go anywhere near SQL (identifiers can't be bound as
// parameters); internal tables (sessions, migrations, `_`-prefixed) are hidden;
// secret-shaped columns (password/hash/token/secret) are masked in the UI and
// excluded from CSV export; the `users` table is read-only.
package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"app/internal/auth"
)

// adminPageSize is rows per page in the table grid.
const adminPageSize = 50

// hiddenTables never appear in the admin: framework internals, not site data.
var hiddenTables = map[string]bool{"sessions": true, "schema_migrations": true}

// readOnlyTables can be browsed but not modified from the admin.
var readOnlyTables = map[string]bool{"users": true}

// maskedColumn reports whether a column's values must never be displayed.
func maskedColumn(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"password", "hash", "token", "secret"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// quoteIdent quotes an SQL identifier (already validated against the schema).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// adminTables lists the site's visible tables, in sqlite_master order.
func (s *Server) adminTables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, "sqlite_") || strings.HasPrefix(name, "_") || hiddenTables[name] {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// adminColumns returns a validated table's column names, or ok=false if the
// table isn't one the admin exposes. This is the gate every handler passes
// user-supplied table names through.
func (s *Server) adminColumns(ctx context.Context, table string) (cols []string, ok bool) {
	tables, err := s.adminTables(ctx)
	if err != nil {
		return nil, false
	}
	found := false
	for _, t := range tables {
		if t == table {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, false
		}
		cols = append(cols, name)
	}
	return cols, len(cols) > 0
}

// formatCell renders a scanned SQLite value for display. Unix-second integers
// in *_at columns become readable timestamps.
func formatCell(col string, v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case int64:
		if strings.HasSuffix(strings.ToLower(col), "_at") && x > 1e9 && x < 4e9 {
			return time.Unix(x, 0).UTC().Format("2006-01-02 15:04")
		}
		return strconv.FormatInt(x, 10)
	default:
		return fmt.Sprint(x)
	}
}

// requireOwner gates a route to the site owner; others get a 404 (the admin's
// existence is not advertised).
func (s *Server) requireOwner(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !u.IsAdmin {
			http.NotFound(w, r)
			return
		}
		next(w, r, u)
	}
}

// --- Views ---

type adminTableEntry struct {
	Name string
	Rows int
}

type adminIndexView struct {
	Tables []adminTableEntry
}

type adminCol struct {
	Name   string
	Masked bool
}

type adminRow struct {
	Rowid int64
	Cells []string
}

type adminTableView struct {
	Table      string
	Columns    []adminCol
	Rows       []adminRow
	Page       int
	PrevPage   int
	NextPage   int
	HasPrev    bool
	HasNext    bool
	ReadOnly   bool
	Hooks      []hookInfo
	OwnerEmail string
}

type adminField struct {
	Name   string
	Value  string
	Masked bool
}

type adminRowView struct {
	Table    string
	Rowid    int64
	Fields   []adminField
	ReadOnly bool
}

// --- Handlers ---

// handleAdmin lists every visible table with its row count.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	tables, err := s.adminTables(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := adminIndexView{}
	for _, t := range tables {
		var n int
		if err := s.db.QueryRowContext(r.Context(),
			`SELECT count(*) FROM `+quoteIdent(t)).Scan(&n); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		v.Tables = append(v.Tables, adminTableEntry{Name: t, Rows: n})
	}
	s.render(w, http.StatusOK, "admin", s.view(r, "Site admin", v))
}

// handleAdminTable renders one table as a paged grid, newest first.
func (s *Server) handleAdminTable(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	table := r.PathValue("table")
	cols, ok := s.adminColumns(r.Context(), table)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	if page < 1 {
		page = 1
	}

	q := `SELECT rowid, * FROM ` + quoteIdent(table) +
		` ORDER BY rowid DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(r.Context(), q, adminPageSize+1, (page-1)*adminPageSize)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	v := adminTableView{Table: table, Page: page, PrevPage: page - 1, NextPage: page + 1,
		HasPrev: page > 1, ReadOnly: readOnlyTables[table]}
	for _, c := range cols {
		v.Columns = append(v.Columns, adminCol{Name: c, Masked: maskedColumn(c)})
	}
	for rows.Next() {
		vals := make([]any, len(cols)+1) // rowid + columns
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		row := adminRow{Rowid: vals[0].(int64)}
		for i, c := range cols {
			cell := "•••••"
			if !maskedColumn(c) {
				cell = formatCell(c, vals[i+1])
				if len(cell) > 120 {
					cell = cell[:120] + "…"
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		v.Rows = append(v.Rows, row)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(v.Rows) > adminPageSize {
		v.Rows = v.Rows[:adminPageSize]
		v.HasNext = true
	}
	if hks, err := s.tableHooks(r.Context(), table); err == nil {
		v.Hooks = hks
	}
	v.OwnerEmail = s.ownerEmail
	view := s.view(r, table+" — Site admin", v)
	view.Flash = r.URL.Query().Get("msg")
	s.render(w, http.StatusOK, "admin_table", view)
}

// handleAdminRow shows one row in full.
func (s *Server) handleAdminRow(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	table := r.PathValue("table")
	cols, ok := s.adminColumns(r.Context(), table)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rowid, err := strconv.ParseInt(r.PathValue("rowid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	q := `SELECT ` + quotedList(cols) + ` FROM ` + quoteIdent(table) + ` WHERE rowid = ?`
	if err := s.db.QueryRowContext(r.Context(), q, rowid).Scan(ptrs...); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := adminRowView{Table: table, Rowid: rowid, ReadOnly: readOnlyTables[table]}
	for i, c := range cols {
		f := adminField{Name: c, Masked: maskedColumn(c)}
		if f.Masked {
			f.Value = "•••••"
		} else {
			f.Value = formatCell(c, vals[i])
		}
		v.Fields = append(v.Fields, f)
	}
	s.render(w, http.StatusOK, "admin_row", s.view(r, table+" row — Site admin", v))
}

// handleAdminRowDelete deletes one row (CSRF-protected; not on read-only tables).
func (s *Server) handleAdminRowDelete(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	table := r.PathValue("table")
	if _, ok := s.adminColumns(r.Context(), table); !ok || readOnlyTables[table] {
		http.NotFound(w, r)
		return
	}
	rowid, err := strconv.ParseInt(r.PathValue("rowid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`DELETE FROM `+quoteIdent(table)+` WHERE rowid = ?`, rowid); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/t/"+table, http.StatusSeeOther)
}

// handleAdminCSV exports a table as CSV — visible (unmasked) columns only.
func (s *Server) handleAdminCSV(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	table := r.PathValue("table")
	cols, ok := s.adminColumns(r.Context(), table)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var visible []string
	for _, c := range cols {
		if !maskedColumn(c) {
			visible = append(visible, c)
		}
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+quotedList(visible)+` FROM `+quoteIdent(table)+` ORDER BY rowid`)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+table+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(visible)
	for rows.Next() {
		vals := make([]any, len(visible))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return // headers already sent; just stop
		}
		rec := make([]string, len(visible))
		for i, c := range visible {
			rec[i] = formatCell(c, vals[i])
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// quotedList joins column names as quoted identifiers.
func quotedList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}
