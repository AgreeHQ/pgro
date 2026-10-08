// pgro is a read-only Postgres query proxy.
//
// `pgro serve` runs in a terminal you control. It reads the connection string
// from a hidden prompt (or a pipe), holds it only in memory, and listens on a
// Unix socket. `pgro query` sends SQL to that socket and prints the result. The
// client never sees the credentials.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usage = `pgro - read-only Postgres query proxy

Usage:
  pgro serve  [flags]          Start the daemon (prompts for the connection string)
  pgro query  [flags] [SQL]    Run one read-only statement (SQL from args or stdin)
  pgro status [flags]          Show what the daemon is connected to

Run "pgro <command> -h" for flags.
`

func defaultSocket() string {
	if s := os.Getenv("PGRO_SOCKET"); s != "" {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/pgro.sock"
	}
	return filepath.Join(home, ".pgro", "pgro.sock")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "query", "q":
		err = runQuery(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "pgro:", err)
		os.Exit(1)
	}
}
