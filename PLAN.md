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
| Secrets | `passwdCmd` (e.g. macOS keychain) / `${ENV}` references in config; no plaintext for work DBs. |
| Config format | YAML (sqls-compatible) for now; TOML deferred (see Phase 1). |
| Execution splitting | Own dialect-aware splitter (`internal/sqlsplit`); the completion parser is not used for execution. |
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
- Dev Oracle (19c SE2) as `CADE`: 132 users, ~6k visible tables / 104k columns unfiltered
  (mostly SYS/XDB + sibling schemas with identical table names); `CADE` alone: 552 tables,
  656 views, 11.7k columns, 710 FKs. Hence the allow-list and view support.
- Oracle rejects a trailing `;` on SQL (ORA-00933) but requires it after PL/SQL `END`.

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
password_cmd = "security find-generic-password -s sqlh-oracle_dev -w"
schemas = ["CADE", "CADE500"]

[connections.pg_local]
driver = "postgresql"
dsn = "postgres://admin@localhost:5432/sdsai?sslmode=disable&options=-c%20search_path%3Dcadetst,public"

[connections.scratch]
driver = "sqlite3"
path = "./scratch.db"
```

(Target format sketch. Current config is sqls-style YAML — see `doc/config.example.yml`; TOML deferred.)

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

### Phase 1 — Oracle (pure Go) + SQLite + config ✅ done

- [x] Replace `godror` → `github.com/sijms/go-ora/v2` (pure Go; `BuildUrl` escapes passwords; `params` → URL options; `dataSourceName` = full go-ora URL). Binary links only macOS system frameworks.
- [x] `CADEV1`: completion (tables, views, columns, FK join hints), hover, Execute Query, `cade500.` qualification
- [x] Schema allow-list `schemas: [...]` (generic `SchemaFilterer`; Oracle: empty = login schema; PG: empty = all). `CADE`+`CADE500` switch ≈ 0.8 s, completion ≈ 10 ms. Lazy per-schema loading not needed at this size → dropped.
- [x] Oracle metadata: views + materialized views via `ALL_OBJECTS`; types with length/precision (`VARCHAR2(254)`, `NUMBER(10,2)`); real PK flag (sqls returned a literal `'1'`); `BIN$` recycle-bin objects excluded; `DATA_DEFAULT` (LONG) no longer fetched
- [x] Identifier case: FK/join/referenced-table lookups case-insensitive (`CanonicalTableName`); opt-in `lowercaseIdentifiers` inserts all-upper names lower-case (mixed case untouched)
- [x] Execution splitter `internal/sqlsplit` (replaces parser-based splitting for execution): strips trailing `;` (ORA-00933), SQL*Plus `/` lines, PL/SQL units kept whole with `END;`, Oracle `q'[...]'`, PG `$tag$` quoting, SQLite `CREATE TRIGGER … END;`; statement-at-cursor incl. attached header comments; per-statement line numbers in errors; "PL/SQL block completed"
- [x] SQLite: verified (completion, FK joins, hover, triggers); views now offered as tables
- [x] `reconnectionDB` all-or-nothing (open new → recache → swap → close old); `switchConnections`/`switchDatabase` roll back index/db name on failure; `curDBName` reset on connection switch; loaded config no longer mutated
- [x] `switchConnections` returns "switched connection to …" (status line); unknown alias lists available ones
- [x] `showConnections`: marks current (`*`), describes host/port/service/path, masks passwords in DSNs, shows `schemas=`
- [x] Secrets: `passwdCmd` (run at connect time) and `${NAME}` env expansion in `user`/`passwd`/`dataSourceName` (bare `$NAME` stays literal). `oracle_dev` password moved to the macOS keychain (`sqlh-oracle_dev`).
- [x] `doc/config.example.yml`; Helix test files `scratch/oracle.sql`, `scratch/sqlite.sql`
- [ ] ~~TOML config~~ → deferred: YAML works and is what sqls users know. Revisit in Phase 2 when `results_dir`/`default_limit` and per-project overrides are added.
- [ ] Drop MySQL/MSSQL/H2/Vertica/ClickHouse drivers (binary 52 MB)? Undecided, cosmetic — build tags if wanted.

Known limitations (not blocking):
- FK join hints only for the default (login) schema; FK cache is keyed by table name only, and sibling schemas (`CADE500`, `CHEMALERT*`) share table names.
- Synonyms aren't completed (CADE has 2; other schemas rely on them heavily).
- Oracle `DATE`/`TIMESTAMP` render as RFC 3339 UTC (`2026-10-12T16:37:20Z`) in result tables → decide formatting in Phase 2 (CSV).

### Phase 1.5 — formatting safety ✅ done

- [x] Fix: lexer stored `''` inside string literals as a single `'`, so every re-render
      (formatter; upstream's executeQuery via `stmt.String()`) corrupted literals like
      `'4,4''-mdi'` → `'4,4'-mdi'`. Now kept doubled. Affects all dialects.
- [x] Format guard (`sqlsplit.FormatHazards`, dialect-agnostic): skip formatting and show a
      warning for Oracle `/` lines, `q'…'` strings, PL/SQL blocks, PG `$$` bodies, and trigger
      bodies (BEGIN/DECLARE). Transaction `BEGIN;` and body-less PG triggers still format.
- Notes: `:format` = whole document via LSP; range formatting (`=`) is a no-op stub in
  sqls. Highlighting is Helix tree-sitter, not the LSP (a stale user grammar in
  `~/.config/helix/runtime/grammars` had broken it; rebuilt).

### Phase 2 — exec engine ⏳ next

- [ ] `internal/exec`: preview (row cap) and full (streamed) modes, `encoding/csv` writer; reuse `internal/sqlsplit`
- [ ] Value formatting for CSV: dates/timestamps (local, no `T…Z`), NULL vs empty, LOBs, RAW/bytea
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

- **2026-10-12** Phase 1 complete. Oracle via go-ora against `CADEV1` (completion incl.
  views and FK joins, hover, execute incl. PL/SQL), SQLite verified, PG unchanged. Schema
  allow-list, all-or-nothing connection switching, execution splitter, keychain password.

- **2026-10-12** Phase 1.5: formatter no longer corrupts escaped quotes; skips (with a
  warning) documents containing constructs it would break.

## 6. Environment notes

- Helix: `[language-server.sqlh] command = "sqlh"`, `args = ["-log", "/tmp/sqlh.log"]`
- Binary: `~/.local/bin/sqlh` (`go build -o ~/.local/bin/sqlh .` from repo root)
- Config: `~/.config/sqlh/config.yml` (example: `doc/config.example.yml`); connections
  `pg_local`, `oracle_dev` (keychain `sqlh-oracle_dev`, schemas CADE+CADE500), `sqlite_scratch`
- Test files: `scratch/test.sql` (PG), `scratch/oracle.sql`, `scratch/sqlite.sql`
- SQLite scratch DB: `/tmp/sqls-build/scratch.db` (customer/orders/big_orders)
- Local PG: Docker `llm-sds-reader-postgres-1`, `admin`/`verysecret`, db `sdsai`, schema `cadetst`
- Oracle dev: see `connection.toml` (gitignored); port 1521 reachable
- Protocol test client: `/tmp/sqls-build/lspc.py` (+ `t_*.py` scenario scripts); Oracle
  repo probe: `scratch/oradbg/` (reads creds from env `H S U P`)
