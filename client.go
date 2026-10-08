package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

func call(socket string, req request, wait time.Duration) (response, error) {
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return response{}, fmt.Errorf("no daemon on %s; start one with `pgro serve` in your own terminal", socket)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(wait))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return response{}, err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return response{}, fmt.Errorf("reading response: %w", err)
	}
	return resp, nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socket := fs.String("socket", defaultSocket(), "Unix socket path (env PGRO_SOCKET)")
	fs.Parse(args)

	resp, err := call(*socket, request{Op: "status"}, 5*time.Second)
	if err != nil {
		return err
	}
	fmt.Println(resp.Status)
	return nil
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	socket := fs.String("socket", defaultSocket(), "Unix socket path (env PGRO_SOCKET)")
	format := fs.String("f", "table", "output format: table, expanded, csv, json")
	timeout := fs.Duration("timeout", 0, "statement_timeout (default and cap set by the daemon)")
	maxRows := fs.Int("max-rows", 200, "rows to return (capped by the daemon)")
	width := fs.Int("w", 200, "truncate table cells to this many characters (0 = no limit)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: pgro query [flags] [SQL | -]\n\nReads SQL from stdin when no SQL argument is given or it is \"-\".\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	sql := strings.Join(fs.Args(), " ")
	if sql == "" || sql == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		sql = string(b)
	}
	sql = strings.TrimSpace(sql)
	sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	if sql == "" {
		return errors.New("no SQL given")
	}

	req := request{Op: "query", SQL: sql, TimeoutMS: int(timeout.Milliseconds()), MaxRows: *maxRows}
	resp, err := call(*socket, req, 2*time.Minute)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}

	switch *format {
	case "json":
		return printJSON(resp)
	case "csv":
		return printCSV(resp)
	case "expanded", "x":
		printExpanded(resp)
	default:
		printTable(resp, *width)
	}
	return nil
}

func footer(resp response) {
	note := ""
	if resp.Truncated {
		note = ", TRUNCATED (raise -max-rows or add LIMIT/ORDER BY)"
	}
	fmt.Printf("(%d rows, %dms%s)\n", len(resp.Rows), resp.ElapsedMS, note)
}

func cell(v *string, width int) string {
	if v == nil {
		return "NULL"
	}
	s := strings.NewReplacer("\n", `\n`, "\t", " ").Replace(*v)
	if width > 0 && utf8.RuneCountInString(s) > width {
		s = string([]rune(s)[:width-1]) + "…"
	}
	return s
}

func printTable(resp response, width int) {
	widths := make([]int, len(resp.Columns))
	for i, c := range resp.Columns {
		widths[i] = utf8.RuneCountInString(c)
	}
	cells := make([][]string, len(resp.Rows))
	for r, row := range resp.Rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			cells[r][i] = cell(v, width)
			widths[i] = max(widths[i], utf8.RuneCountInString(cells[r][i]))
		}
	}
	line := func(vals []string) {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(v))
		}
		fmt.Println(strings.TrimRight(strings.Join(parts, " | "), " "))
	}
	line(resp.Columns)
	seps := make([]string, len(widths))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	fmt.Println(strings.Join(seps, "-+-"))
	for _, row := range cells {
		line(row)
	}
	footer(resp)
}

func printExpanded(resp response) {
	nameWidth := 0
	for _, c := range resp.Columns {
		nameWidth = max(nameWidth, utf8.RuneCountInString(c))
	}
	for r, row := range resp.Rows {
		fmt.Printf("-[ RECORD %d ]-\n", r+1)
		for i, v := range row {
			fmt.Printf("%-*s | %s\n", nameWidth, resp.Columns[i], cell(v, 0))
		}
	}
	footer(resp)
}

func printCSV(resp response) error {
	w := csv.NewWriter(os.Stdout)
	w.Write(resp.Columns)
	for _, row := range resp.Rows {
		rec := make([]string, len(row))
		for i, v := range row {
			if v != nil {
				rec[i] = *v
			}
		}
		w.Write(rec)
	}
	w.Flush()
	if resp.Truncated {
		fmt.Fprintln(os.Stderr, "pgro: output truncated")
	}
	return w.Error()
}

func printJSON(resp response) error {
	rows := make([]map[string]*string, len(resp.Rows))
	for r, row := range resp.Rows {
		obj := make(map[string]*string, len(row))
		for i, v := range row {
			obj[resp.Columns[i]] = v
		}
		rows[r] = obj
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"rows": rows, "truncated": resp.Truncated, "elapsed_ms": resp.ElapsedMS})
}
