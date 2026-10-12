# sqlh — SQL LSP + query runner for Helix

Fork of [sqls](https://github.com/sqls-server/sqls) (MIT). Repo: https://github.com/raikhan/sqlh
(`origin`), upstream remote `upstream` for rebasing parser fixes.

This file is the working plan and progress log. Original analysis is condensed in §1–§3;
status lives in §4–§5.

## 1. Decisions (settled)

| Topic | Decision |
|---|---|
| Base | Fork sqls, don't rewrite. Parser/completer/hover/formatter are the asset (~19k LOC, tested). |
| Name | `sqlh` (binary, module `github.com/raikhan/sqlh`, config `~/.config/sqlh/config.yml`). |
| Shape | Hybrid: the LSP process owns connection + schema cache + live buffers; same operations exposed via (a) LSP `executeCommand`/code actions and (b) a `sqlh` CLI over a Unix socket, with standalone fallback. |
| Oracle driver | `sijms/go-ora/v2` (pure Go) instead of `godror` (needs Oracle Instant Client; confirmed failing with `DPI-1047` on this machine). |
| "Current connection" scope | **Per workspace.** Socket keyed by workspace root; CLI resolves it by walking up from cwd (`SQLH_SOCK` overrides). |
| Results location | **In-project** `{workspace}/.sqlh/results/`, gitignored. `{conn}_{stamp}.csv` + `latest.csv` symlink. |
| Oracle metadata | **Schema allow-list** per connection (`schemas = [...]`); lazy per-schema loading. Never scan `ALL_TABLES`/`ALL_TAB_COLUMNS` unfiltered. |
| Opening results | **Save first, don't auto-open.** Viewing (vd in a Herdr pane) is wired on the Helix/Nu side later. |
| Preview limit | Client-side row cap (read N rows, stop) — dialect-agnostic, safe for CTEs/ORDER BY. Server-side `FETCH FIRST`/`LIMIT` rewriting optional later. |
| Secrets | `password_cmd` / env-var references in config; no plaintext for work DBs. |
| Git | User pushes; agent commits only when asked. Commit email for this repo: GitHub no-reply. |

## 2. Key findings (why things are the way they are)

- sqls already had Oracle code (`internal/database/oracle.go`); only the driver is the problem.
- Helix (25.07) **discards** the result of `workspace/executeCommand`, never sends a document
  URI with `:lsp-workspace-command`, and can't prompt for command arguments. It **does**
  support `:lsp-workspace-command cmd "json" args`, code actions with selection range,
  `window/showMessage` (status line), `window/showDocument`, and `$/progress`.
- sqls is built around the connection's **default schema** (`current_schema()`): FK/join
  completion, `SortedTables()`, `ColumnDescs()` are keyed to it. For PG set `search_path` in
  the DSN; for Oracle the login schema is the default, and cross-schema needs the allow-list.
- Helix config traps that broke the original PG attempt: connections must arrive via
  `initializationOptions.connectionConfig` or a config file; driver name is `postgresql`.
- The sqls JSON-RPC handler is synchronous: server→client *requests* (`showDocument`) must be
  issued from a goroutine or they deadlock.
- `reconnectionDB` closes the old connection before opening the new one → a failed switch
  leaves a dead connection (completion still works from cache; execute fails). To fix.

## 3. Architecture

```
                ┌──────────────────────────────────────────────┐
  Helix ──stdio─┤  sqlh lsp                                    │
   │            │  • parser / completer / hover / format       │
   │ :pipe-to   │  • connection registry  (current = "dev")    │
   │ :sh        │  • schema cache (lazy, per schema allow-list)│
   ▼            │  • exec engine: stmt extract → run → CSV     │
  sqlh CLI ─────┤  • unix socket JSON-RPC (sourcegraph/jsonrpc2)│
  (standalone   └──────────────┬───────────────────────────────┘
   fallback)                   │ go-ora / pgx / sqlite3
                               ▼
                       Oracle · Postgres · SQLite
```

### Config target (`~/.config/sqlh/config.toml`, overridable by `.sqlh.toml` up from workspace root)

```toml
default = "pg_local"
results_dir = "{workspace}/.sqlh/results"
results_name = "{conn}_{stamp}.csv"
default_limit = 100

[connections.oracle_dev]
driver = "oracle"
host = "oracle.dev.example"
port = 1521
service = "CADEV1"
user = "cade"
password_cmd = "security find-generic-password -s cadev1 -w"
schemas = ["CADE", "CADE_RPT"]

[connections.pg_local]
driver = "postgresql"
dsn = "postgres://admin@localhost:5432/sdsai?sslmode=disable&options=-c%20search_path%3Dcadetst,public"

[connections.scratch]
driver = "sqlite3"
path = "./scratch.db"
```

(Current config is still sqls' YAML format; migrating to the TOML above is part of Phase 1.)

### Control plane (Phase 3)

| CLI | LSP `executeCommand` | effect |
|---|---|---|
| `sqlh connect [alias]` (none → fzf picker) | `switchConnections "alias"` | switch + re-cache; completions follow |
| `sqlh connections` / `sqlh status` | `showConnections` | list, mark current |
| `sqlh run [--limit N] [--file F --line L] [--out P]` | `sqlh.run 100` | preview → CSV |
| `sqlh export ...` (= `run --limit 0`) | `sqlh.export` | full run → CSV, streamed |
| `sqlh cancel` | `sqlh.cancel` | abort running query |
| `sqlh refresh` | `sqlh.refresh` | re-cache schema |

Statement selection priority: stdin SQL → `{file,line}` against the server's **live buffer**
(no save needed) → selection/statement-under-cursor from code action → whole document.

### Helix glue (Phase 4, sketch)

```toml
[keys.normal.space.q]
r = [":pipe-to sqlh run --limit 100 --file r##'%{buffer_name}'## --line %{cursor_line}", ":sh ~/.config/helix/helix-action.nu errors"]
R = [":pipe-to sqlh export --file r##'%{buffer_name}'## --line %{cursor_line}", ":sh ~/.config/helix/helix-action.nu errors"]
v = [":sh ~/.config/helix/open-in-herdr-pane.nu 0.9 vd (sqlh latest)"]
c = [":sh ~/.config/helix/open-in-herdr-pane.nu 0.3 sqlh connect"]
s = [":lsp-workspace-command showConnections"]
```

## 4. Phases

### Phase 0 — validate the base with PG ✅ done

- [x] Build stock sqls; config file; raw JSON-RPC smoke test (completion against `sdsai.cadetst`)
- [x] Wire into Helix (`languages.toml`), test file `scratch/test.sql`
- [x] Fix: advertise `executeCommandProvider` (Helix refused to run commands)
- [x] Fix: alias/column completion for schema-qualified tables; `join schema.` → tables
- [x] Fix: exit panic (`close of closed channel`)
- [x] Execute Query from Helix: carry code-action range; selection or statement-under-cursor;
      strip leading comments; default to last-used document
- [x] Results to editor: one-liners → `showMessage`; multi-line → `$TMPDIR/sqlh/<cmd>.txt` +
      `showDocument(takeFocus)`; errors → `showMessage`
- [x] Hover: aligned fenced table instead of markdown pipes
- [x] PG `search_path` in DSN → FK join completion works
- [x] Fork → `raikhan/sqlh`, module/binary/config renamed, binary untracked, `master` tracks `origin`
- [x] `oracle_dev` added to config; `switchConnections` mechanics verified (fails only on driver)

### Phase 1 — Oracle (pure Go) + SQLite + config ⏳ next

- [ ] Replace `godror` → `github.com/sijms/go-ora/v2`; DSN builder (`oracle://user:pass@host:port/service`, TNS alias via `TNS_ADMIN`)
- [ ] Connect to `CADEV1`; completion (tables/columns/FK joins), hover, Execute Query
- [ ] Schema allow-list (`schemas`) for Oracle metadata; lazy per-schema cache; measure startup time
- [ ] Oracle quirks: identifier case (upper-case metadata vs. lower-case typing), `DUAL`, `FETCH FIRST`, PL/SQL `/` terminator handling in statement splitting
- [ ] SQLite: verify against a scratch `.db` (should already work)
- [ ] Fix `reconnectionDB` ordering (open new → close old; keep previous index on failure)
- [ ] `showConnections`: description fallback when no `proto` (currently blank for Oracle)
- [ ] Config: TOML format above, `password_cmd` / `${ENV}` references; keep YAML reading for a transition
- [ ] Decide: drop MySQL/MSSQL/H2/Vertica/ClickHouse drivers from the build (binary is 43 MB) — keep behind build tags?

### Phase 2 — exec engine

- [ ] `internal/exec`: preview (row cap) and full (streamed) modes, `encoding/csv` writer
- [ ] `results_dir` / `results_name` templating, `latest.csv` symlink, `.sqlh/` gitignore hint
- [ ] Run in goroutine with cancellable context; `$/progress`; `cancel` command
- [ ] Summary (`rows, elapsed, path`) via `showMessage`; no auto-open
- [ ] DML/DDL: rows affected; autocommit default; explicit transactions pass through
- [ ] Expose as `executeCommand` (`sqlh.run <limit>`, `sqlh.export`) + code actions

### Phase 3 — control plane + CLI

- [ ] Unix socket per workspace (`$XDG_RUNTIME_DIR`/`~/.cache/sqlh/<hash>.sock`), JSON-RPC
- [ ] `sqlh` subcommands: `lsp`, `connect`, `connections`, `status`, `run`, `export`, `cancel`, `refresh`, `latest`
- [ ] Standalone fallback (no server → open connection from config directly)
- [ ] Socket resolution from cwd; `SQLH_SOCK` override

### Phase 4 — Helix / Herdr glue

- [ ] `languages.toml` + `space q` keymap
- [ ] `sqlh.nu` helper: fzf connection picker in a Herdr pane, `vd` on `latest.csv`, limit prompt
- [ ] Decide `showDocument` focus behaviour (`takeFocus` vs background load) for non-CSV outputs

### Phase 5 — later / optional

- [ ] `EXPLAIN` to buffer
- [ ] Server-side LIMIT rewriting for simple SELECTs
- [ ] `modernc.org/sqlite` (cgo-free static binary), pgx v5
- [ ] Upstream PRs to sqls-server/sqls: `executeCommandProvider`, schema-qualified alias
      columns, `join schema.`, exit panic (generic, non-controversial)
- [ ] README rewrite for sqlh

## 5. Progress log

- **2026-10-12** Phase 0 complete. PG (`sdsai.cadetst`, Docker) working end-to-end in Helix:
  completion, hover, FK join completion, Execute Query via `space a`, command output via
  `showDocument`. Repo forked/renamed to `sqlh`. `oracle_dev` configured; blocked only on
  `godror` needing Instant Client → go-ora swap is next.

## 6. Environment notes

- Helix: `[language-server.sqlh] command = "sqlh"`, `args = ["-log", "/tmp/sqlh.log"]`
- Binary: `~/.local/bin/sqlh` (`go build -o ~/.local/bin/sqlh .` from repo root)
- Config: `~/.config/sqlh/config.yml` (sqls YAML format until Phase 1 migrates it)
- Test file: `scratch/test.sql` (statements must end with `;`)
- Local PG: Docker `llm-sds-reader-postgres-1`, `admin`/`verysecret`, db `sdsai`, schema `cadetst`
- Oracle dev: see `connection.toml` (gitignored); port 1521 reachable
- Protocol test harnesses (ad hoc): `/tmp/sqls-build/{smoke2,e2e2,sw}.py`
