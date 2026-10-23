# pgro

A read-only Postgres query proxy. You give the credentials to a daemon in your
own terminal. An LLM agent (or anything else) sends SQL to the daemon through a
Unix socket and never sees the credentials.

```
your terminal                         agent shell
┌─────────────────────────┐           ┌──────────────────────────────┐
│ pgro serve              │◄──socket──│ pgro query "select ..."      │
│  DSN in memory only     │  (0600,   └──────────────────────────────┘
│  prints every query     │  0700 dir)
└───────────┬─────────────┘
            │ new connection per query, BEGIN READ ONLY, statement_timeout
            ▼
        Postgres (dedicated read-only role)
```

## Layers of protection

In order of importance:

1. **The Postgres role.** This is the guard that Postgres itself enforces. At
   startup the daemon refuses any role that is SUPERUSER, CREATEROLE, CREATEDB,
   BYPASSRLS, or REPLICATION. It also refuses a role that is a member of
   `pg_signal_backend`, can CREATE in the database or in any schema, or has
   INSERT, UPDATE, DELETE, or TRUNCATE on any table.
2. **The credentials stay in memory.** The daemon reads the DSN from a hidden
   prompt or from a pipe. It never takes the DSN from a flag or an env var,
   because other processes can read argv and the environment with `ps`. It never
   prints connection-string parse errors, because they can echo the password.
3. **Each query runs in its own transaction and connection.** The daemon opens a
   fresh connection with `default_transaction_read_only=on`, then runs
   `BEGIN READ ONLY`, `SET LOCAL statement_timeout` and `lock_timeout`, the query,
   and `ROLLBACK`. No session state carries over to the next query.
4. **One statement per request.** The daemon uses the extended protocol, so
   Postgres rejects `select 1; delete ...`.
5. **A keyword allowlist.** Only SELECT, WITH, EXPLAIN, SHOW, TABLE, and VALUES
   are allowed. The parser skips comments, including nested ones. This check is
   a convenience; layers 1, 3, and 4 are the real guards.
6. **Limits.** A row cap, a timeout cap, a limit of 2 queries at once, and a TTL
   (8h by default).
7. **Visibility.** Every query is printed in your terminal and appended to
   `~/.pgro/audit.log`. The agent can edit that file, so trust the terminal.

### Remaining risk

The daemon runs as your macOS user, and so does the agent's shell. macOS stops
one process from reading another process's memory without root or debugger
rights, but the two processes share a user. For full isolation, run the daemon
as a separate macOS user. See "Hardening" below.

## Install

```sh
cd ~/src/pgro && go build -o /usr/local/bin/pgro .   # Go 1.24+ (1.22.3 links broken binaries on this macOS)
```

## 1. Create the role (run manually on prod, as an admin)

Choose one option. **Option A is the recommended default when an LLM reads the
output.**

### Option A: stats only (no table data, no PII)

```sql
CREATE ROLE pgro_stats LOGIN PASSWORD '<generate one>' CONNECTION LIMIT 3;
GRANT pg_monitor TO pgro_stats;  -- full pg_stat_statements text, pg_stat_*, pg_settings, pg_locks
ALTER ROLE pgro_stats SET default_transaction_read_only = on;
ALTER ROLE pgro_stats SET statement_timeout = '30s';
```

With this role you can read `pg_stat_statements`, `pg_stat_user_tables`,
`pg_stat_user_indexes`, `pg_statio_*`, `pg_stat_activity`, `pg_locks`,
`pg_settings`, `pg_indexes`, and table and index sizes. Most query texts are
parameterized (`$1`), but SQL with inline literals will show those values.

You cannot run `EXPLAIN`, because it needs SELECT on the table, and you cannot
read `pg_stats` column values.

### Option B: read all data (EXPLAIN works, but the output can include PII)

```sql
-- Option A, plus:
GRANT pg_read_all_data TO pgro_stats;  -- PostgreSQL 14+
```

`EXPLAIN (ANALYZE, BUFFERS)` works with this role. But `select * from users`
then sends customer data to whatever reads the output. To grant this access only
for a short time, run `GRANT` before a session and `REVOKE` after it.

## 2. Start the daemon (your terminal)

```sh
pgro serve                                       # paste the DSN at the hidden prompt
op read "op://Private/prod-pgro/dsn" | pgro serve  # or pipe it from 1Password
```

For `pg_stat_statements`, connect to the **primary**. Each server keeps its own
statistics, so a replica shows only its own traffic.

## 3. Query (agent or you)

```sh
pgro status
pgro query "select calls, mean_exec_time, query from pg_stat_statements order by total_exec_time desc limit 20"
pgro query -f expanded < some.sql
pgro query -f csv -max-rows 2000 "..."
pgro query -timeout 45s "..."
```

The output formats are `table` (cells truncated to `-w` characters), `expanded`,
`csv`, and `json`.

### Claude Code permissions

To let an agent run queries without a prompt each time, add this rule to
`.claude/settings.local.json` or your user settings:

```json
{ "permissions": { "allow": ["Bash(pgro query:*)", "Bash(pgro status)"] } }
```

Do not allow `pgro serve`. Only you start the daemon.

## Hardening (optional): a separate macOS user

```sh
sudo dscl . -create /Users/_pgro UserShell /usr/bin/false   # or create the user in System Settings
sudo mkdir -p /var/run/pgro && sudo chown _pgro:staff /var/run/pgro && sudo chmod 750 /var/run/pgro
op read "op://Private/prod-pgro/dsn" | sudo -u _pgro pgro serve -socket /var/run/pgro/pgro.sock
```

After this, your user can no longer read the daemon's memory or files. The
socket directory check needs `chmod 700`; extend it to allow group access if
you use this layout.
