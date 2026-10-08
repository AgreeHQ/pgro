package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/term"
)

type server struct {
	cfg        *pgconn.Config
	target     string
	maxRows    int
	maxTimeout time.Duration
	defTimeout time.Duration
	slots      chan struct{}
	audit      *log.Logger
	startedAt  time.Time
	expiresAt  time.Time
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := fs.String("socket", defaultSocket(), "Unix socket path (env PGRO_SOCKET)")
	maxRows := fs.Int("max-rows", 2000, "hard cap on rows returned per query")
	maxTimeout := fs.Duration("max-timeout", 60*time.Second, "hard cap on statement_timeout")
	defTimeout := fs.Duration("timeout", 15*time.Second, "default statement_timeout")
	concurrency := fs.Int("concurrency", 2, "max queries running at once")
	ttl := fs.Duration("ttl", 8*time.Hour, "exit after this long (0 = never)")
	auditPath := fs.String("audit-log", "", "append every query here (default: <socket dir>/audit.log)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: pgro serve [flags]

Reads the connection string from a hidden prompt, or from stdin when piped:
  op read "op://Vault/prod-ro/dsn" | pgro serve

The connection string is never accepted as a flag or env var, because other
processes can read those.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	dsn, err := readDSN()
	if err != nil {
		return err
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// pgconn errors can echo the DSN, password included. Never print them.
		return errors.New("could not parse the connection string")
	}
	// pgconn forwards any query parameter it does not know (e.g. a GUI client's
	// driverVersion=...) to the server as a setting, and Postgres rejects unknown
	// settings. Keep only the settings a read-only session needs.
	keep := map[string]string{}
	for k, v := range cfg.RuntimeParams {
		switch k {
		case "search_path", "timezone", "TimeZone", "DateStyle", "IntervalStyle", "client_encoding":
			keep[k] = v
		default:
			fmt.Fprintf(os.Stderr, "pgro: ignoring connection string parameter %q\n", k)
		}
	}
	cfg.RuntimeParams = keep
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["application_name"] = "pgro"
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}

	s := &server{
		cfg:        cfg,
		maxRows:    *maxRows,
		maxTimeout: *maxTimeout,
		defTimeout: min(*defTimeout, *maxTimeout),
		slots:      make(chan struct{}, *concurrency),
		startedAt:  time.Now(),
	}
	if *ttl > 0 {
		s.expiresAt = s.startedAt.Add(*ttl)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	target, err := s.verifyRole(ctx)
	cancel()
	if err != nil {
		return err
	}
	s.target = target

	dir := filepath.Dir(*socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Some platforms ignore socket file modes, so the directory is the boundary.
	if info, err := os.Stat(dir); err != nil {
		return err
	} else if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket directory %s is %s; it must be private (chmod 700)", dir, info.Mode().Perm())
	}
	if *auditPath == "" {
		*auditPath = filepath.Join(dir, "audit.log")
	}
	af, err := os.OpenFile(*auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer af.Close()
	s.audit = log.New(af, "", log.LstdFlags|log.LUTC)

	ln, err := listen(*socket)
	if err != nil {
		return err
	}
	defer os.Remove(*socket)

	fmt.Fprintf(os.Stderr, "pgro: connected as %s\n", s.target)
	fmt.Fprintf(os.Stderr, "pgro: listening on %s (max %d rows, timeout %s/%s max)\n", *socket, s.maxRows, s.defTimeout, s.maxTimeout)
	if !s.expiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "pgro: exits at %s; Ctrl-C to stop sooner\n", s.expiresAt.Format(time.Kitchen))
	}
	s.audit.Printf("serve start target=%s", s.target)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var expire <-chan time.Time
	if !s.expiresAt.IsZero() {
		expire = time.After(time.Until(s.expiresAt))
	}
	go func() {
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "\npgro: stopping")
		case <-expire:
			fmt.Fprintln(os.Stderr, "pgro: ttl reached, stopping")
		}
		ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				s.audit.Printf("serve stop")
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

func readDSN() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Connection string (input hidden): ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
	if err != nil {
		return "", err
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", errors.New("no connection string on stdin")
	}
	return dsn, nil
}

func listen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("a daemon is already listening on %s", path)
		}
		os.Remove(path)
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o600)
}

// verifyRole refuses to serve a role that could change anything. The read-only
// transaction is a second layer; the role is the layer Postgres guarantees.
const roleCheckSQL = `
SELECT current_user || '@' || coalesce(host(inet_server_addr()), 'local') || '/' || current_database()
         || ' (PostgreSQL ' || current_setting('server_version') || ')',
       r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR r.rolreplication,
       has_database_privilege(current_database(), 'CREATE'),
       pg_has_role('pg_signal_backend', 'USAGE'),
       EXISTS (SELECT 1 FROM pg_namespace n
               WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
                 AND has_schema_privilege(n.oid, 'CREATE')),
       EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
               WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
                 AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
                 AND (has_table_privilege(c.oid, 'INSERT') OR has_table_privilege(c.oid, 'UPDATE')
                      OR has_table_privilege(c.oid, 'DELETE') OR has_table_privilege(c.oid, 'TRUNCATE')))
FROM pg_roles r WHERE r.rolname = current_user`

func (s *server) verifyRole(ctx context.Context) (string, error) {
	conn, err := pgconn.ConnectConfig(ctx, s.cfg)
	if err != nil {
		return "", fmt.Errorf("connect failed: %s", safeErr(err))
	}
	defer conn.Close(context.Background())

	results, err := conn.Exec(ctx, "BEGIN READ ONLY;"+roleCheckSQL+";ROLLBACK").ReadAll()
	if err != nil {
		return "", fmt.Errorf("role check failed: %s", safeErr(err))
	}
	if len(results) != 3 || len(results[1].Rows) != 1 {
		return "", errors.New("role check returned no row")
	}
	row := results[1].Rows[0]
	target := string(row[0])
	checks := []struct {
		val  []byte
		what string
	}{
		{row[1], "has SUPERUSER, CREATEROLE, CREATEDB, BYPASSRLS, or REPLICATION"},
		{row[2], "can CREATE in the database"},
		{row[3], "is a member of pg_signal_backend (can cancel other sessions)"},
		{row[4], "can CREATE in a schema"},
		{row[5], "has INSERT, UPDATE, DELETE, or TRUNCATE on a table"},
	}
	var problems []string
	for _, c := range checks {
		if string(c.val) == "t" {
			problems = append(problems, "  - "+c.what)
		}
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("refusing to serve %s; the role:\n%s\nUse a dedicated read-only role (see README)", target, strings.Join(problems, "\n"))
	}
	return target, nil
}

func (s *server) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))

	var req request
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(c, 1<<20))).Decode(&req); err != nil {
		json.NewEncoder(c).Encode(response{Error: "bad request: " + err.Error()})
		return
	}
	c.SetReadDeadline(time.Time{})

	var resp response
	switch req.Op {
	case "status":
		resp = response{Status: s.status()}
	case "query":
		resp = s.query(req)
	default:
		resp = response{Error: fmt.Sprintf("unknown op %q", req.Op)}
	}
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	json.NewEncoder(c).Encode(resp)
}

func (s *server) status() string {
	st := fmt.Sprintf("connected as %s\nup %s, max %d rows, timeout %s (max %s)",
		s.target, time.Since(s.startedAt).Round(time.Second), s.maxRows, s.defTimeout, s.maxTimeout)
	if !s.expiresAt.IsZero() {
		st += fmt.Sprintf("\nexits in %s", time.Until(s.expiresAt).Round(time.Minute))
	}
	return st
}

func (s *server) query(req request) response {
	start := time.Now()
	resp := s.execute(req)
	resp.ElapsedMS = time.Since(start).Milliseconds()

	outcome := fmt.Sprintf("%d rows", len(resp.Rows))
	if resp.Truncated {
		outcome += " (truncated)"
	}
	if resp.Error != "" {
		outcome = "ERROR " + resp.Error
	}
	fmt.Fprintf(os.Stderr, "%s  %-60s → %s, %dms\n", time.Now().Format("15:04:05"), oneLine(req.SQL, 60), outcome, resp.ElapsedMS)
	s.audit.Printf("query elapsed_ms=%d result=%q sql=%q", resp.ElapsedMS, outcome, req.SQL)
	return resp
}

func (s *server) execute(req request) response {
	kw := leadingKeyword(req.SQL)
	if !allowedKeywords[kw] {
		return response{Error: fmt.Sprintf("only SELECT, WITH, EXPLAIN, SHOW, TABLE, and VALUES are allowed (got %q)", kw)}
	}

	timeout := s.defTimeout
	if req.TimeoutMS > 0 {
		timeout = min(time.Duration(req.TimeoutMS)*time.Millisecond, s.maxTimeout)
	}
	maxRows := s.maxRows
	if req.MaxRows > 0 {
		maxRows = min(req.MaxRows, s.maxRows)
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-time.After(timeout):
		return response{Error: "daemon busy; try again"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout+15*time.Second)
	defer cancel()

	// A fresh connection per query: no session state carries between queries.
	conn, err := pgconn.ConnectConfig(ctx, s.cfg)
	if err != nil {
		return response{Error: "connect: " + safeErr(err)}
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn.Close(closeCtx)
		closeCancel()
	}()

	setup := fmt.Sprintf("BEGIN READ ONLY; SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = 2000",
		timeout.Milliseconds())
	if _, err := conn.Exec(ctx, setup).ReadAll(); err != nil {
		return response{Error: "begin: " + safeErr(err)}
	}

	// ExecParams uses the extended protocol, so Postgres rejects more than one
	// statement. With no result formats given, every value comes back as text.
	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()
	rr := conn.ExecParams(qctx, req.SQL, nil, nil, nil, nil)

	var resp response
	for _, fd := range rr.FieldDescriptions() {
		resp.Columns = append(resp.Columns, fd.Name)
	}
	for rr.NextRow() {
		if len(resp.Rows) == maxRows {
			resp.Truncated = true
			qcancel() // stop the server streaming the rest; the connection is discarded
			break
		}
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v != nil {
				str := string(v)
				row[i] = &str
			}
		}
		resp.Rows = append(resp.Rows, row)
	}
	if _, err := rr.Close(); err != nil && !resp.Truncated {
		return response{Error: safeErr(err)}
	}
	if !resp.Truncated {
		conn.Exec(ctx, "ROLLBACK").ReadAll()
	}
	return resp
}

// safeErr returns the Postgres error text. Connection errors from pgconn may
// include host and user but never the password; parse errors are not passed here.
func safeErr(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		msg := pgErr.Severity + ": " + pgErr.Message
		if pgErr.Detail != "" {
			msg += "\nDETAIL: " + pgErr.Detail
		}
		if pgErr.Hint != "" {
			msg += "\nHINT: " + pgErr.Hint
		}
		return msg
	}
	return err.Error()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
