# sqlh — SQL LSP + query runner for Helix

Fork of [sqls](https://github.com/sqls-server/sqls) (MIT). Repo: https://github.com/raikhan/sqlh
(`origin`), upstream remote `upstream` for rebasing parser fixes.

This file is the working plan and progress log. Original analysis is condensed in §1–§3;
status lives in §4–§5.

## 1. Decisions (settled)

| Topic                      | Decision                                                                                                                                                                                                    |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Base                       | Fork sqls, don't rewrite. Parser/completer/hover/formatter are the asset (~19k LOC, tested).                                                                                                                |
| Name                       | `sqlh` (binary, module `github.com/raikhan/sqlh`, config `~/.config/sqlh/config.yml`).                                                                                                                      |
| Shape                      | Hybrid: the LSP process owns connection + schema cache + live buffers; same operations exposed via (a) LSP `executeCommand`/code actions and (b) a `sqlh` CLI over a Unix socket, with standalone fallback. |
| Oracle driver              | `sijms/go-ora/v2` (pure Go) instead of `godror` (needs Oracle Instant Client; confirmed failing with `DPI-1047` on this machine).                                                                           |
| "Current connection" scope | **Per workspace.** Socket keyed by workspace root; CLI resolves it by walking up from cwd (`SQLH_SOCK` overrides).                                                                                          |
| Results location           | **In-project** `{workspace}/.sqlh/results/`, gitignored. `{conn}_{stamp}.csv` + `latest.csv` symlink.                                                                                                       |
| Oracle metadata            | **Schema allow-list** per connection (`schemas = [...]`); lazy per-schema loading. Never scan `ALL_TABLES`/`ALL_TAB_COLUMNS` unfiltered.                                                                    |
| Opening results            | **Save first, don't auto-open.** Viewing (vd in a Herdr pane) is wired on the Helix/Nu side later.                                                                                                          |
| Preview limit              | Client-side row cap (read N rows, stop) — dialect-agnostic, safe for CTEs/ORDER BY. Server-side `FETCH FIRST`/`LIMIT` rewriting optional later.                                                             |
| Secrets                    | `passwdCmd` (e.g. macOS keychain) / `${ENV}` references in config; no plaintext for work DBs.                                                                                                               |
| Config format              | YAML (sqls-compatible) for now; TOML deferred (see Phase 1).                                                                                                                                                |
| Execution splitting        | Own dialect-aware splitter (`internal/sqlsplit`); the completion parser is not used for execution.                                                                                                          |
| Git                        | User pushes; agent commits only when asked. Commit email for this repo: GitHub no-reply.                                                                                                                    |

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
- The sqls JSON-RPC handler is synchronous: server→client _requests_ (`showDocument`) must be
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

| CLI                                                  | LSP `executeCommand`        | effect                                |
| ---------------------------------------------------- | --------------------------- | ------------------------------------- |
| `sqlh connect [alias]` (none → fzf picker)           | `switchConnections "alias"` | switch + re-cache; completions follow |
| `sqlh connections` / `sqlh status`                   | `showConnections`           | list, mark current                    |
| `sqlh run [--limit N] [--file F --line L] [--out P]` | `sqlh.run 100`              | preview → CSV                         |
| `sqlh export ...` (= `run --limit 0`)                | `sqlh.export`               | full run → CSV, streamed              |
| `sqlh cancel`                                        | `sqlh.cancel`               | abort running query                   |
| `sqlh refresh`                                       | `sqlh.refresh`              | re-cache schema                       |

Statement selection priority: stdin SQL → `{file,line}` against the server's **live buffer**
(no save needed) → selection/statement-under-cursor from code action → whole document.

### Helix glue (Phase 4, sketch)

```toml
[keys.normal.space.q]
r = [
  ":pipe-to sqlh run --limit 100 --file r##'%{buffer_name}'## --line %{cursor_line}",
  ":sh ~/.config/helix/helix-action.nu errors",
]
R = [
  ":pipe-to sqlh export --file r##'%{buffer_name}'## --line %{cursor_line}",
  ":sh ~/.config/helix/helix-action.nu errors",
]
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
- [x] Verified formatting (`internal/formatter/verify.go`, `FormatVerified`). A 24-case
      battery showed the raw sqls formatter drops comments, alters quoted identifiers, edits
      half-written SQL, panics on `::`/`->>`/`(+)`/`:bind`/`@link`/`b.`, and oscillates
      between layouts. Now every `:format` result is checked before it's applied:
      - hazards skipped (above); formatter panics recovered → skip
      - spacing inside statements canonicalised from the token sequence first (layout-
      independent input; adjacency kept where meaningful: name+`(`, word+string, `.`, ops)
      - result must have identical tokens to the source (comments/strings/quoted identifiers
      exact, words case-insensitive, operators unsplit), else skip
      - fixed point: up to 3 verified passes until formatting again changes nothing
      - layout between statements (blank lines, `; -- note`, leading/trailing) restored from source
      - one whole-document edit (Helix diffs it → cursor stays); no edit when unchanged
      Result on the battery: 9 formatted, 1 unchanged, 13 skipped with a reason, 0 corrupted.
- [x] Fix: formatter panicked whenever the parser left an operand nil — typed literals
      (`d > DATE '2026-04-01'`), half-typed SQL (`b.`, `cadetst.`, `x =`). That was the
      cause of every formatter crash in the battery. Formatter omits missing operands;
      `ast.joinString/joinRender` skip nil nodes. Battery now: 14 formatted, 1 unchanged,
      8 skipped, 0 crashes. The remaining "alter content" skips are real formatter bugs the
      verifier catches: `-1, .5`→`-1.5`, `->>`→`- > >`, `limit 10offset`, `priorempno`
      (candidates for Phase 5 fixes).
      Safe for format-on-save (worst case: no-op + status-line warning).
- [x] Fix: Oracle config validation required `proto`/`passwd`/`port` → rejected `passwdCmd`
      configs; with `oracle_dev` first in the config sqlh failed to start.
- Notes: `:format` = whole document via LSP; range formatting (`=`) is a no-op stub in
  sqls. Highlighting is Helix tree-sitter, not the LSP (a stale user grammar in
  `~/.config/helix/runtime/grammars` had broken it; rebuilt).

### Phase 2 — exec engine ✅ done

- [x] `internal/exec` (no LSP dependency, reusable by the CLI): `Run` streams one statement to a
      `Sink` (`CSVFile`, `Table`); row cap with "more available" detection (reads N+1);
      query/exec classification incl. `… RETURNING`; zero-column results; rows affected only for DML
- [x] Value formatting, based on probing go-ora/pgx/go-sqlite3: Oracle NUMBER / PG numeric are
      exact strings (no float rounding); wall-clock `TIMESTAMP`/Oracle `DATE` without fake `Z`;
      PG/SQLite `DATE` date-only (when midnight); `…TZ` keeps offset; RAW/BLOB/BYTEA → `0x…`;
      JSON as text; NULL → `results.null` (default ""). Note Oracle returns `''` as NULL.
- [x] Files: `results.dir` (default `{workspace}/.sqlh/results`; `{workspace}` = LSP rootUri,
      else the document's dir), `{conn}_{yyyymmdd-HHMMSS}[_n][-k].csv`, written to `.part` and
      renamed on success (cancel/error removes it), `latest.csv` relative symlink (atomic),
      `.sqlh/.gitignore` = `*` (project .gitignore untouched)
- [x] Jobs: one background job at a time (LSP stays responsive); `$/progress` begin/report
      (row count ~1/s)/end; `sqlh.cancel`; switching connection cancels the job first
      ("cancelled (connection switched)"); second run while busy is rejected with a hint
- [x] Pinned session (`*sql.Conn`) per connection for all executions: temp tables,
      `begin … commit/rollback` and session settings carry across runs (psql/sqlplus-like).
      Pinged before each run, reopened if broken. Switching connection closes it (an open
      transaction is rolled back by the server).
- [x] Summary via `showMessage`: `oracle_dev: 100 rows (more available) → .sqlh/results/….csv · 129ms`;
      DML `3 rows affected`; PL/SQL `PL/SQL block completed`; multi-statement totals; errors with
      line number; no auto-open
- [x] Commands/code actions: `Run → CSV (first N rows)` (`sqlh.run`), `Export → CSV (all rows)`
      (`sqlh.export`), `Show results (text, first N rows)` (`executeQuery`, now async + capped),
      `Cancel: …` (`sqlh.cancel`, listed only while a job runs). `results.previewLimit` (100).
- Verified end to end: PG 2M-row export 1.3 s; Oracle `caproduct` 291,245 rows 8 s; Oracle
  server-side cancel (ORA-01013) and mid-fetch cancel ~10 ms with session reusable; PG
  cancel immediate; temp table + transaction across runs; switch during a job 0.07 s.
- Limitations: a custom row limit needs the CLI (Phase 3) — Helix can't pass the cursor with
  `:lsp-workspace-command`, so `sqlh.run`/`sqlh.export` require a range (code actions provide it).
  Progress shows nothing while Oracle is still executing (no rows yet); the spinner still runs.

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
  warning) documents containing constructs it would break. Then: verified formatting
  (token-identical, stable, layout between statements kept) → format-on-save is safe.

- **2026-10-12** Phase 2 complete: CSV preview/export via code actions, background jobs with
  progress and cancel, pinned session, exact value formatting, `.sqlh/results` + `latest.csv`.

## 6. Environment notes

- Helix: `[language-server.sqlh] command = "sqlh"`, `args = ["-log", "/tmp/sqlh.log"]`
- Binary: `~/.local/bin/sqlh` (`go build -o ~/.local/bin/sqlh .` from repo root)
- Config: `~/.config/sqlh/config.yml` (example: `doc/config.example.yml`); connections
  `oracle_dev` (default; keychain `sqlh-oracle_dev`, schemas CADE), `pg_local`, `sqlite_scratch`
- Test files: `scratch/test.sql` (PG), `scratch/oracle.sql`, `scratch/sqlite.sql`
- SQLite scratch DB: `/tmp/sqls-build/scratch.db` (customer/orders/big_orders)
- Local PG: Docker `llm-sds-reader-postgres-1`, `admin`/`verysecret`, db `sdsai`, schema `cadetst`
- Oracle dev: see `connection.toml` (gitignored); port 1521 reachable
- Protocol test client: `/tmp/sqls-build/lspc.py` (+ `t_*.py` scenario scripts;
  `t_fmtsafety2.py` = formatter battery); Oracle
  repo probe: `scratch/oradbg/` (reads creds from env `H S U P`)
