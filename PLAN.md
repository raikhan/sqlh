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

### Phase 3 — control plane + CLI ✅ done

- [x] `internal/control`: JSON-RPC over a per-workspace Unix socket
      (`~/Library/Caches/sqlh/<hash>.sock`, `$XDG_RUNTIME_DIR/sqlh`, `SQLH_RUNTIME_DIR`), sidecar
      `<hash>.json` (workspace, pid). Key = symlink-resolved workspace root (macOS `/tmp` →
      `/private/tmp` broke the first version). First server wins; stale sockets replaced; clear
      error if the path exceeds the 104-byte socket limit. Removed on shutdown.
- [x] Server: starts the socket at `initialize`; all request handling (editor + CLI) serialised
      by `Server.mu`; control connections use an async handler so a waiting `run` still notices
      the CLI disconnecting (Ctrl-C, kill -9 → job cancelled); `run` waits for its job without
      holding the lock (editor completion stays ~1 ms during a CLI export)
- [x] Jobs report a structured `RunResult`; CLI-started runs also show progress + summary in the
      editor. Sources: SQL (stdin/`--sql`) or `--file`+`--line` against the editor's **live buffer**
      (unsaved edits), else disk. `--out`, `--table`, `--json`, `--limit`.
- [x] CLI (`internal/cmd`): `lsp` (default), `run`, `export`, `connect [alias]` (fzf picker),
      `connections`/`ls`, `status [--all]`, `cancel`, `refresh`, `latest`; stdin ≤ 1 char ignored
      (Helix `:pipe-to` cursor). Reference: `doc/cli.md`.
- [x] Standalone fallback (no server, or `--conn`/`--standalone`): config connection (`--conn`,
      else remembered, else first), workspace = nearest `.git`/`.helix`/… ancestor, same results
      dir/naming/summary; Ctrl-C cancels; progress on a TTY
- [x] Remembered connection per workspace (`~/.local/state/sqlh/state.json`), restored at editor
      start — each project reopens on its last database
- [x] LSP command `sqlh.refresh`; shared `internal/report` (summaries, text tables) for server + CLI;
      `database.DescribeConnection`
- [x] Fix: `exit` called `Stop()` re-entrantly (would deadlock with the new lock) → `stopLocked`
- Verified: unit tests (+ `-race`), full CLI↔server scenario on a `-race` server build with no
  race reports; PG + Oracle (keychain) standalone.

### Phase 3.5 — JSON key completion + CLI pickers ✅ done

- [x] JSON keys completion (`internal/jsoncomplete`), keys sampled from the data (1,000 non-null
      rows), cached 10 min per column/path, cleared on switch/refresh; 1.5 s cap per lookup, then
      `isIncomplete` and the fetch finishes in the background. `'` added as a trigger character.
  - PostgreSQL json/jsonb: `col->'…'`, `->>'…'`, nested chains incl. array indexes
    (`->'ingredients'->0->>'…'`), arrays looked through (`->'ingredients'->>'…'`), `#>`/`#>>`
    `'{a,…'`, `?`, and `jsonb_path_*(col, '$.a.…')`. `jsonb_each` gives types and frequency
    (`hazchemCode: string|null · 6/6 sampled`).
  - Oracle 19c: `json_value/json_query/json_exists/json_table(col, '$.…')` on any column (no
    `IS JSON` constraints in CADE; CLOB JSON in e.g. `CAREGLISTENTRY.CONTENT_JSON`, 500k rows) via
    `JSON_DATAGUIDE` (~70 ms); keys needing quotes inserted as `."CAS RN"`.
  - Column resolution: FROM-clause aliases (string closed at the cursor first, so an
    un-paired quote doesn't swallow the FROM), else a unique JSON column of that name.
  - Inside ordinary string literals completion now returns nothing (was keyword noise).
  - Array-element keys (PG operator styles): `->'ghsItems'->>'code'` is always NULL, yet
    looking through arrays offered `code` there. Now flagged `⚠ array element key`, sorted
    last, with a doc popup giving `->0->>'code'`, `jsonb_path_query_array(col, '$.ghsItems[*].code')`
    and `jsonb_array_elements`. Not flagged with an index or in SQL/JSON paths (lax mode).
- [x] CLI catalog + pickers (`internal/catalog`, `internal/cmd/{catalog,picker}.go`): `tables`,
      `columns`, `schemas`, `databases`, `db`; list (one insertable name per line), `--long`,
      `--json`, `-p` fzf pick (table preview = columns; columns multi-select with `--prefix`/`--sep`).
      fzf like `fzf-in-helix.nu` (height mode, kitty protocol off) and no trailing newline into a
      pipe → works with `:insert-output`. Server cache (instant) or standalone (queries).
      Names as typed: default schema unqualified, Oracle upper-case lowered with
      `lowercaseIdentifiers`, quoted when needed (`database.QuoteIdent`).
- [x] Fix: urfave/cli ignored flags after a positional (`sqlh columns t -p`) → `cmd.ReorderArgs`
- [x] Fix (upstream sqls bug): switching database on a DSN-configured PostgreSQL connection was a
      silent no-op (DSN's dbname won) → `pgDSNWithDB`; Oracle/SQLite `db` now explain instead of
      failing with ORA-12514

- [x] Fix: formatting refused queries using PostgreSQL operators (`raw_data->'language'`): the
      lexer had no multi-character operators (`->>` = Minus Gt Gt → formatted `- > >`). New
      `token.Operator` for `-> ->> #> #>> #- @> <@ @? @@ ?| ?& || !~ !~*` (`!~` used to be a
      tokenizer error). Spacing: `->`/`->>` and `::` tight, the rest spaced. A lone `@` (Oracle
      dblink, MySQL @var), `?` (bind placeholder) and `#` (Oracle names) unchanged; `tags@>'{}'`
      splits correctly (one-token pushback). Verifier tokenizer taught the same, so a glued
      `@>`/`#>` can't be mis-split unnoticed. Battery: 14 formatted, 9 skipped, 0 corrupted.
- [x] Fix (pre-existing): after a JSON operator expression, a cast (`x::text`) or `||` in the
      select list, completion for the next `alias.` fell back to keywords — the parser only knew
      `+ - * / % ^` as operators, so the select list wasn't recognised as a list. Parser now builds
      Operator nodes for `token.Operator` and `::` (cast right side may be a type keyword);
      `formatOperator` keeps `->`/`->>`/`::` tight.

### Phase 4 — Helix / Herdr glue

- [x] `languages.toml` + `space i` keymap (`space q` is taken): `r` run, `v` run → VisiData,
      `l` limit prompt, `e` export, `o` open latest, `x` cancel, `t`/`c` insert table/columns,
      `C`/`D` connection/database pickers, `s` status, `u` refresh
- [x] `~/.config/helix/sqlh.nu` helper, status line only (`:sh` popups stick until Esc):
      queries start detached (`trap "" HUP` + setsid, no inherited pipes) from `:pipe-to`; the
      server reports progress/summary/errors to the editor, including errors before the job
      starts (`controlRun` → `notifyEditorError`); cancel/refresh via `:lsp-workspace-command`;
      VisiData in a 70% Herdr pane via `open-in-herdr-pane.nu`; pickers under `:pipe-to` /
      `:insert-output` + `:redraw`; limit prompt = fzf presets or a typed number (`10` is 10,
      not the `100` preset), `all` = export
- [x] `sqlh notify [--error] MESSAGE` (hidden, control method `sqlh/notify`): Helix treats a
      failed command's stderr as output (`:insert-output` would insert the error), so the
      helper exits with empty stderr and relays the message to the status line
- [ ] Decide `showDocument` focus behaviour (`takeFocus` vs background load) for non-CSV outputs

### Diagnostics (schema-aware lint, no database round-trips)

- [x] `internal/lint`: per statement (sqlsplit + sqls lexer), against a catalog snapshot of the
      schema cache. Reports (as errors, source `sqlh`):
      - unknown qualifier: `fm.*` / `fm.col` with no `fm` among the statement's tables/aliases
        (`in scope: fd`); Oracle only when the member is `*` or a column in scope (else it may be
        `pkg.function`)
      - unknown table in FROM/JOIN/INTO/UPDATE/USING/DELETE, with "did you mean"
      - unknown column on a resolved table: `alias.col`, INSERT column list, UPDATE SET
- Quiet when unsure: scope = all tables of the statement (subqueries included, so correlated
  references never trip); skips PL/SQL, DDL, templates (`${x}`, `{{x}}`, `{x}`), `$$`/q-quotes,
  SQL*Plus `&var`, dblinks, schemas not in the cache, table functions/CTEs/derived tables
  (columns unknown), `alias(col, …)`, PARTITION/SAMPLE/PIVOT/AS OF…; Oracle unqualified names
  only once synonyms are loaded (`ALL_SYNONYMS`, PUBLIC + allowed schemas, background load) and
  the current schema is cached; PostgreSQL: table found in any one schema (search_path unknown),
  materialized views now in the table list; tables created/altered anywhere in the document
  (including `EXECUTE IMMEDIATE '…'`) known / columns unchecked; pseudo-columns (ROWID, ctid…)
- Handler: debounced 300 ms per document, off the request lock; re-run for all open documents
  when the cache changes (connect, switch, refresh, background load); cleared on close.
  `diagnostics: false` in the config turns it off.
- Validated on real SQL: 1060 chemalert Oracle files (24k statements) → every finding an object
  really missing from CADE (other schemas/versions); hand-written queries 0 findings; PG
  llm-sds-reader 0 false positives. 490 KB script: 64 ms.

### Phase 6 — completion depth (after a lazydb comparison)

Probe of 16 positions against lazydb's LSP (same `pg_local`): sqlh ahead everywhere except
function completion (`DataBaseFunctions` returned `[]` for PG/SQLite and Oracle's *reserved
words*), and both failed on CTE columns. A wider probe found more subquery bugs.

- [x] Scope resolution rewrite (`parser/parseutil/scope.go`): query levels = statement +
      enclosing subqueries; each level sees its own `FROM`/`JOIN` (not those of nested
      subqueries, CTE bodies, function arguments like `extract(year from …)`), the branch of a
      `UNION`/`INTERSECT`/`EXCEPT`/`MINUS` containing the cursor, and the enclosing levels'
      tables as `Outer` (qualified use only). Replaces the sqls heuristics (first `FROM` of the
      whole statement; subqueries *after* the cursor), which leaked a CTE body's tables into the
      main query and returned nothing for CTE references.
- [x] Select-list columns never fail: functions/expressions/casts/`CASE` with an alias,
      `DISTINCT [ON]`, `TOP n`, `*` and `t.*` (expanded through derived tables/CTEs), origins
      tracked through nesting (`cid` ← `city.id`). Before: a `count(*) as n` in a subquery made
      the whole completion request fail.
- [x] CTEs: `WITH [RECURSIVE] x [(cols)] AS [[NOT] MATERIALIZED] (…)`, also `x(cols)` without
      a space; referenced/aliased in FROM/JOIN; CTEs built on CTEs; CTE names in `FROM` ranked
      first; `x.` filtered to that relation (before: all subqueries' columns for any parent).
- [x] `FULL [OUTER] JOIN`, `NATURAL … JOIN`, `LATERAL`, `ONLY` recognised.
- [x] Functions: `dialect.Function` + curated built-ins (PG 128, Oracle 96, SQLite 77 entries,
      text tables in `dialect/functions.go`), `FunctionLister` loaded with the background
      cache (PG `pg_proc` minus I/O/operator/support/stats internals: 1256 signatures in 9 ms;
      Oracle `ALL_ARGUMENTS` of the allowed schemas minus object-type methods: 210 on CADE),
      merged in `database.FunctionCatalog` (curated summary first, exact overloads after).
      Completion only with a typed prefix (or after `pkg.`), snippet `name($0)`; Oracle names
      lower-cased with `lowercaseIdentifiers`.
- [x] Signature help for calls: lexer-light scan (`completer.FunctionCallAt`; strings,
      quoted identifiers and comments skipped, works on unclosed calls), overloads with
      parameter offsets (`[start, end]`, so `(text, text)` highlights the right one), optional
      groups `a [, b]` split into `[b]`; variadic last parameter stays active.
- [x] Hover/docs for CTE and derived-table columns (type + origin column); go-to-definition
      for WITH query names. `SubqueryDoc` (nil `ParentTable` panic) replaced by `RelationDoc`.
- Verified: unit tests (+ `-race`), live PG + Oracle (CADE packages, PL/SQL `begin pkg.|`),
  formatter battery output byte-identical.
- [x] Fix: JSON key completion on a derived-table/CTE column (`tmp.raw_data->>'…'`): the
      qualifier is resolved through the relation to its origin table column (also renamed
      columns, unqualified, CTEs); expressions stay unresolved.
- [x] Formatter: `j->>'k' like('%x%')` was glued (`'k'like(`): a call/name after an operator
      expression gets a space; predicate keywords keep a space before `(` (`in ('a')`,
      `like ('x')`, `exists (…)` — `in (` used to lose it); `BETWEEN a AND b` stays on one line
      (the AND used to break like a condition; goldens updated).
- [x] Fix: an indented first statement stayed indented — `restoreGaps` copied the document's
      leading whitespace verbatim; now only its newlines (like the gaps between statements).

### Phase 7 — relationships between tables (DBeaver-like explorer, in fzf)

Decided: no separate TUI; the table picker and `describe` cover it. Schemas are boundaries
(no cross-schema relationships). A wrong connection is worse than none: guesses are labelled
"maybe" and strict.

- [x] Relationship graph of the default schema (`internal/database/relgraph.go`,
      `DBCache.Relations`): declared FKs; view lineage (a view column that is a plain column
      reference inherits that column's FKs; a view exposing a table's PK joins back to it;
      views on views resolved down to tables; `UNION` views get none); guesses (`maybe`):
      a column named `<table>_id`/`<table>id` for a single-column PK `id`, or the PK's own
      name when it starts with the table name (`customer.customer_id`), only without a
      declared FK, exactly one candidate table, same type family; view dependencies (`uses`).
      View-derived edges are listed for the view only (`ViewOnly`): with them on the table side,
      CADE's `CAPRODSYNONYM` listed ~130 views exposing `CAPRODUCT.PROD_ID` (14k edges).
- [x] Views: `ViewDescriber` — PG `pg_get_viewdef` + `pg_depend` via `pg_rewrite`, Oracle
      `ALL_VIEWS.TEXT`/`ALL_MVIEWS.QUERY` (LONG, fetched whole; 656 CADE views in 0.25 s) +
      `ALL_DEPENDENCIES`, SQLite `sqlite_master.sql` (dependencies parsed). Loaded with the
      background cache; graph rebuilt then (CADE: parse 1.2 s — the sqls parser —, 16.9k edges;
      582/595 non-UNION views with lineage, 5062/6841 columns).
- [x] FK queries: PG from `pg_constraint` (information_schema matched constraints by name,
      unique only per table), same schema only; Oracle `R_OWNER = OWNER`; SQLite
      `REFERENCES t` without columns (NULL `to`, used to fail the scan) → the parent's PK.
      PG materialized view columns (not in information_schema) from `pg_attribute`.
- [x] Scope: parenthesised joins `FROM (a JOIN b ON …)` (how PG prints view definitions)
      were invisible to completion and lineage; `parseutil.ExtractQueryLineage`.
- [x] LSP: join completion from the graph (view edges "view column from X", guesses "maybe",
      sorted after FKs; `relations.maybe: false` hides them; a self-referencing FK was offered
      twice), table/view kind in completion detail and hover, hover lists relationships,
      the tables a view reads and the views reading a table.
- [x] CLI: `describe TABLE` (picker preview), `relations TABLE`; control methods
      `sqlh/relations`, `sqlh/context` (statement at a cursor: after JOIN?, tables before it,
      table under the cursor, indentation). Table picker `tables -p --file --line --col`:
      related tables first; after JOIN Enter inserts `t alias ON …` (`relations.insert`,
      `--insert`; Ctrl-T the other); Ctrl-R drills into a table's relationships (state file
      updated by the hidden `_nav` through fzf `transform`), accepting inserts the join chain;
      Ctrl-O back. Standalone works too (builds the graph per call: slower).
- [x] Helix `space i t` passes `--file/--line/--col` (`%{cursor_column}`).
- Verified: unit tests, live PG (test schema with views/matview/UNION), Oracle CADE, SQLite;
  real fzf driven through a pty (drill, back, Ctrl-T, Esc).
- [x] Picker cleanup (feedback): the list showed `--with-nth=2..` of 3-field lines (name +
      marker + condition, truncated) → names only (`--with-nth=3`, only names matched), the
      join and the hover-style description in the preview (`_nav preview`); other tables
      dimmed; a table listed twice shows the anchor's columns. Preview wrapped with fzf's `↳`
      → `wrap-word` + indented wrap sign. Cursor on a statement's table → starts with its
      relationships, and accepting copies the name to the clipboard (pbcopy/wl-copy/xclip/
      xsel) with a status-line note instead of inserting into the name under the cursor. All pickers dropped `--no-sort` (typing ranks by match quality; the
      input order holds while the query is empty).
- [x] Relationship lines `→ other.col = col  notes` (related table first, once) in hover,
      `describe` and the preview; hover marks the table as inline code (coloured like the
      title; markdown hard breaks instead of an aligned code block), the preview in cyan
      with dimmed notes.
- [ ] Lineage for `UNION` views when every branch takes a column from the same table column.
- [x] Views in the LSP: go to definition of a view or a view's column opens a read-only file
      with its `CREATE` statement (`<runtime dir>/views/<workspace key>/<conn>/<schema>.<view>.sql`; `control.Find` maps
      that directory back to the workspace's server, so the Helix glue works there; the
      column's line found in the first select list; Helix reuses the running server for it,
      so hover/gd work inside). `ViewDef.Writes`: PG `pg_relation_is_updatable(oid, true)`
      (triggers, rules), Oracle `ALL_UPDATABLE_COLUMNS` (0.16 s for 656 CADE views) +
      `INSTEAD OF` triggers + `ALL_MVIEWS.UPDATABLE`, SQLite `INSTEAD OF` triggers; a
      diagnostic for INSERT/UPDATE/DELETE/MERGE into a view that refuses it, "read-only"/
      "no INSERT" in hover and `describe`. Test views in pg_local: `scratch/views_pg.sql`
      (`v_batch_document` read-only, `v_excluded_document` on it, `v_openai_model`
      updatable, `mv_batch_summary`). CADE: 513 read-only, 143 writable views.
- [ ] Go to definition of a table: generated DDL (columns, keys) the same way.

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

- **2026-10-12** Phase 3 complete: CLI + per-workspace control socket, standalone mode,
  remembered connection per workspace.

- **2026-10-12** Phase 3.5: JSON key completion (PG operators/jsonpath, Oracle SQL/JSON via
  dataguide) and CLI pickers for tables/columns/schemas/databases.

- **2026-10-13** Phase 6: scope rewrite (CTEs, derived tables, UNION branches, correlated
  subqueries), function completion + signature help from curated built-ins and the database.

- **2026-10-13** Phase 7: relationship graph (FKs, view lineage, "maybe" guesses, view
  dependencies), `describe`/`relations`, cursor-aware table picker with JOIN clauses and
  drill-down, join completion and hover for views. Go to definition of views (read-only
  definition files), diagnostics for writes to read-only views and materialized views.

## 6. Environment notes

- Helix: `[language-server.sqlh] command = "sqlh"`, `args = ["-log", "/tmp/sqlh.log"]`
- Binary: `$GOPATH/bin/sqlh` (`just install` from repo root); CLI: `doc/cli.md`
- Tests isolating sockets/state: `SQLH_RUNTIME_DIR=/tmp/sqlh-rt XDG_STATE_HOME=/tmp/sqlh-state`;
  `/tmp/sqls-build/t_p3.py` (CLI↔server scenario)
- Config: `~/.config/sqlh/config.yml` (example: `doc/config.example.yml`); connections
  `oracle_dev` (default; keychain `sqlh-oracle_dev`, schemas CADE), `pg_local`, `sqlite_scratch`
- Test files: `scratch/test.sql` (PG), `scratch/oracle.sql`, `scratch/sqlite.sql`
- SQLite scratch DB: `/tmp/sqls-build/scratch.db` (customer/orders/big_orders)
- Local PG: Docker `llm-sds-reader-postgres-1`, `admin`/`verysecret`, db `sdsai`, schema `cadetst`
- Oracle dev: see `connection.toml` (gitignored); port 1521 reachable
- Protocol test client: `/tmp/sqls-build/lspc.py` (+ `t_*.py` scenario scripts;
  `t_fmtsafety2.py` = formatter battery); Oracle
  repo probe: `scratch/oradbg/` (reads creds from env `H S U P`)
- Relationships: `CFG=<config> go run ./scratch/relg ALIAS [TABLE…]` prints the graph
  (`MAYBE=1`: all guesses); PG test schema with views: `scratch/relations_pg.sql`;
  views and their writability: `go run ./scratch/viewprobe ALIAS [all]`; pg_local test views
  (cadetst): `scratch/views_pg.sql`;
  scenario scripts `/tmp/sqls-build/t_rel*.py` (`t_rel_pty.py` drives real fzf in a pty)
