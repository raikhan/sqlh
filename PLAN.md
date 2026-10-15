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
| Preview limit              | Client-side row cap (read N rows, stop), statement never rewritten. PostgreSQL through a cursor (`DECLARE … FETCH N+1`): pgx otherwise reads the whole result on close.                                     |
| Secrets                    | `passwdCmd` (e.g. macOS keychain) / `${ENV}` references in config; no plaintext for work DBs.                                                                                                               |
| Config format              | YAML (sqls-compatible), final. TOML dropped: cosmetic gain, nested config fits YAML, sqls users know it.                                                                                                    |
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

### Config

`~/.config/sqlh/config.yml` (sqls-style YAML, or `-config <file>`). Annotated reference:
`doc/config.example.yml`.

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
- [x] ~~TOML config~~ → dropped (2026-10-14): staying on YAML (see Decisions).
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
- [x] PostgreSQL previews through a cursor (`internal/exec/pgcursor.go`). Abandoning the rows
      after N is cheap on go-ora (batched fetch) and SQLite, but PostgreSQL streams the whole
      result and pgx's `rows.Close()` reads it to the end: a 100-row preview of 5M rows took
      2.8 s, as long as the export. Now `DECLARE sqlh_preview NO SCROLL CURSOR FOR <statement>;
      FETCH N+1; CLOSE` (3 ms; first-rows plans via `cursor_tuple_fraction`). Idle session: own
      `BEGIN … COMMIT` (like autocommit); in the user's transaction: declared there, savepoint
      around `DECLARE`; failed transaction or `DECLARE` refused (data-modifying `WITH`,
      `SELECT … INTO`, syntax error): run as written, errors unchanged. Only `SELECT`/`WITH`/
      `VALUES`/`TABLE` with a limit on a pinned session; exports unchanged. Tests need a
      server: `SQLH_TEST_PG=<dsn> go test ./internal/exec`.

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
- [x] Helper folded into the CLI, `sqlh.nu` removed; the bindings call `sqlh` directly:
      `--editor` (errors → status line via the server, nothing on stderr, exit 1 silently via
      `cmd.ErrReported`; summaries suppressed; stdin ignored), `--bg` (re-exec with `Setsid`,
      stdio `/dev/null`, piped SQL passed as `--sql`), `run --limit ask|all|N`, `run --open` /
      `sqlh open` with `results.viewer` (from the server's config via `Status.Viewer`, else the
      file; default `open`/`xdg-open`), `--file` finds the workspace on every command (scratch
      buffer → cwd). Server errors from `sqlh/run` come back as `control.RemoteError` and are
      not shown twice; `controlRun` now notifies all its early errors.

### Diagnostics (schema-aware lint, no database round-trips)

- [x] `internal/lint`: per statement (sqlsplit + sqls lexer), against a catalog snapshot of the
      schema cache. Reports (as errors, source `sqlh`):
      - unknown qualifier: `fm.*` / `fm.col` with no `fm` among the statement's tables/aliases
        (`in scope: fd`); Oracle only when the member is `*` or a column in scope (else it may be
        `pkg.function`)
      - unknown table in FROM/JOIN/INTO/UPDATE/USING/DELETE, with "did you mean"
      - unknown column on a resolved table: `alias.col`, INSERT column list, UPDATE SET
- Quiet when unsure: scope = all tables of the statement (subqueries included, so correlated
  references never trip); skips PL/SQL, templates (`${x}`, `{{x}}`, `{x}`), `$$`/q-quotes,
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

### Phase 8 — query files: connection header, `sqlh dump`, result reuse

Goal: query files run from the terminal (automation) on the connection last chosen in Helix,
CSV written next to the caller (`<file>.csv`); `space i d` does the same from the editor.

Decided:

- **Header** `-- sqlh: conn=ALIAS [db=NAME]` in the file's leading comments (`key=value`,
  other keys kept). `db=` only when the database was switched (`space i D`). No schema: it
  comes from the connection (DSN `search_path`, Oracle login schema); session state
  (`SET search_path`, `ALTER SESSION`) is not carried — put it in the file.
- **The header follows the editor**: the server rewrites (or inserts) it, in open buffers only
  (`workspace/applyEdit`, versioned; the buffer is modified, the CLI sees it after `:w`):
  on every run of a document (code actions, `space i r/e/d`, `sqlh run --file` via the server),
  and on `sqlh connect/db --file F` (Helix `space i C/D` pass the buffer). Files on disk are
  never touched by the CLI.
- **`sqlh dump FILE… [-d DIR | -o FILE] [--line L [--col C]] [--conn ALIAS] [--fresh]`**:
  all rows → `DIR/<stem>.csv` (default: cwd; more result sets `<stem>_2.csv` …), overwritten
  on success only; `latest.csv` untouched. From the terminal: always standalone, the file on
  disk, connection `--conn` › header › **error**; several files each on their own header,
  failures don't stop the others (exit 1 at the end). From Helix (`--editor`, via `sqlh.nu`):
  through the server — editor's connection (header stamped), live buffer, statement at the
  cursor, status line, `space i X` cancels; Helix's cwd is the default directory.
- **`--out` numbering**: also for `run/export --out`: later result sets `<stem>_N.csv` instead
  of the results directory.
- **Reuse** (`space i d` only): the server remembers complete results written to the results
  directory (`e`, or `r` when not truncated) by connection + database + exact statement text
  (leading comments, i.e. the header, are not part of it); a dump of the same statement copies
  that CSV instead of running when: same session generation (bumped by any statement that
  isn't a plain read: DML/DDL/PL/SQL/`BEGIN`/`COMMIT`/…, and on reconnect or switch), younger
  than `results.reuseFor` (default `10m`, `0` disables), the file still exists. `--fresh`
  forces a run. Can't see other sessions' writes: hence the age limit and the status line
  (`reused the result from 14:02`). Terminal dumps never reuse.

- [x] `internal/header`: parse/stamp (CRLF, block comments, quoting, kept keys, UTF-16 ranges)
- [x] `--out` numbering (`exec.NumberedPath`), old file replaced only on success, no `latest.csv`
      for out files; `results.reuseFor` (validated; `0`/bare seconds accepted)
- [x] `sqlh dump` standalone (header required, `--conn`, several files, `--line`, `-d`/`-o`)
- [x] Server: header stamping on runs and `connect/db --file` (versioned `workspace/applyEdit`
      from a goroutine); dump via the server (`RunParams.Dump/Fresh`); reuse (`jobState.results`,
      forgotten by non-read statements via `exec.ReadOnly`, `closeSession`); `RunResult.ReusedFrom`
- [x] Header hint: information diagnostic on the header when it names another target than
      the editor's (warning: unknown connection; independent of `diagnostics: false`), re-checked
      on switches; code actions `Switch the editor to X` (`sqlh.useHeader`: connection, then
      database or back to the connection's own) and `Point the file's header at Y`
      (`sqlh.stampHeader`), first when the cursor is on the header.
- [x] `sqlh.nu dump [--fresh] [--bg]` (shared `detach`), `space i d`, `space i C/D` pass
      `--file`; doc/cli.md (also fixed `space i X/S/U`), README, config example
- Verified: unit tests (header, numbering, copy, `ReadOnly`), CLI dump against SQLite
  in process, server tests with a fake editor applying edits over the real control socket
  (stamp on run/switch, reuse/forget/fresh/truncated/reuseFor 0/reconnect), end to end with
  the built binary: scripted LSP client + `sqlh.nu dump --bg` on pg_local (header inserted,
  status line, reuse after `export`, `--fresh`).

### Phase 9 — DDL awareness (completion, hover, lint, format) ✅ done

Trigger: hands-on test (2026-10-14) — keywords complete at the start of a DDL statement and
nothing after them. Census below from a probe of ~100 cursor positions against the mock
schema (`city`/`country`/`countrylanguage`, handler-level `textDocument/completion` and
`hover`) plus the formatter on typical DDL. "kw" = the dialect's whole keyword list (145
for the default list, ~450 for PostgreSQL), unfiltered by context.

#### 9.1 Census

**Root causes**

1. `parseutil.CheckSyntaxPosition` knows only DML anchors (`SELECT`, `FROM`, `WHERE`, `SET`,
   `JOIN`, `ON`, `INSERT INTO`, `TRUNCATE`, …). Any other position falls to the completer's
   `default` branch: whole keyword list + functions with a prefix. No `TABLE`, `VIEW`,
   `COLUMN`, `REFERENCES`, `RETURNING`, `INDEX … ON`, `COMMENT ON` anchors.
2. `isInsertColumns` is "the cursor is inside any parenthesis": every `(…)` is treated as an
   INSERT column list → columns of the scope's tables, never keywords. In `CREATE TABLE t (`
   the scope table is `t` (not in the cache) → empty list.
3. The scope collector counts `TABLE` as a table keyword, so `ALTER TABLE city …` puts `city`
   in scope (good) and `CREATE TABLE t` puts `t` (unknown) in scope.
4. The parser turns `name(` (no space) into a `FunctionLiteral`: `city(Name)`, `t(id int)`,
   `numeric(12,2)` — position checks then see a function call.
5. `ON` always means a join condition (`getJoinOnCondition`): `CREATE INDEX i ON |`,
   `GRANT … ON |`, `CREATE TRIGGER … ON |` offer columns of an empty scope → nothing.
6. A member identifier in the default branch is treated as `pkg.function`:
   `COMMENT ON COLUMN city.|`, `ALTER TABLE world.|`, `DROP TABLE world.|` → nothing.
7. No data-type list anywhere (types only appear by accident in keyword lists: PG has
   `VARCHAR`/`INTEGER`/`TEXT`, Oracle has no `VARCHAR2`/`NUMBER`, SQLite none).
8. Oracle's keyword list (`oracleKeyWords`, 90 words) lacks `JOIN`, `LEFT`, `INNER`, `MERGE`,
   `USING`, `PRIMARY`, `KEY`, `FOREIGN`, `REFERENCES`, `CONSTRAINT`, `ADD`, `MODIFY`,
   `COLUMN`, `RENAME`, `TRUNCATE`, `COMMENT`, `MATERIALIZED`, `CASCADE`, `PURGE`, … (also
   hurts DML). `oracleReservedWords` (PL/SQL) isn't used for completion.
9. The cache has no index, constraint, sequence, trigger or user-type names.

**Completion — by statement** (✗ broken, ≈ accidental/partly right, ✓ works)

| Position | Today | Expected |
| --- | --- | --- |
| `CREATE TABLE \|`, `… IF NOT EXISTS \|` | kw | nothing / `IF NOT EXISTS`, schema names for `schema.` |
| `CREATE TABLE t (\|` and `(id int, \|` | ✗ empty | column name: nothing; table-constraint keywords (`CONSTRAINT`, `PRIMARY KEY`, `FOREIGN KEY`, `UNIQUE`, `CHECK`, PG `LIKE`, `EXCLUDE`) |
| `CREATE TABLE t (id \|`, `(id va\|` | ✗ empty | data types of the dialect |
| `CREATE TABLE t (id int \|` | ✗ empty | column-constraint keywords (`NOT NULL`, `NULL`, `DEFAULT`, `PRIMARY KEY`, `UNIQUE`, `REFERENCES`, `CHECK`, `GENERATED … AS IDENTITY`, `COLLATE`, Oracle `ENABLE`…) |
| `… DEFAULT \|` | ✗ empty | functions/literals (`now()`, `CURRENT_TIMESTAMP`, `SYSDATE`, `gen_random_uuid()`, `nextval(`) |
| `… REFERENCES \|` (column or table constraint) | ✗ empty | tables |
| `… REFERENCES country (\|`, `country(\|` | ✗ empty | columns of `country` (PK/unique first) |
| `PRIMARY KEY (\|`, `UNIQUE (\|`, `FOREIGN KEY (\|` in CREATE TABLE | ✗ empty | columns defined earlier in the statement |
| `CHECK (\|` in CREATE TABLE | ✗ empty | columns defined in the statement, functions |
| `CREATE TABLE t (LIKE \|` (PG) | ✗ empty | tables |
| `CREATE TABLE t AS SELECT …` | ✓ | — |
| `ALTER TABLE \|`, `ALTER TABLE ci\|` | kw / empty | tables (views for `ALTER VIEW`, matviews for `ALTER MATERIALIZED VIEW`) |
| `ALTER TABLE world.\|` | ✗ empty | tables of `world` |
| `ALTER TABLE city \|` | kw | `ADD`, `DROP`, `ALTER`, `RENAME`, PG `OWNER TO`/`SET`/`ENABLE`…, Oracle `MODIFY` |
| `… DROP COLUMN \|`, `DROP \|` (MySQL/Oracle style), `RENAME COLUMN \|`, `ALTER COLUMN \|` | kw | columns of `city` (+ `COLUMN`/`CONSTRAINT` after a bare `DROP`) |
| `… ALTER COLUMN Name \|` | kw | `TYPE`, `SET`, `DROP` (`DEFAULT`, `NOT NULL`) |
| `… ALTER COLUMN Name TYPE \|`, `ADD COLUMN x \|`, `ADD x \|` | kw | data types |
| `ALTER TABLE city ADD (x \|` (Oracle) | ≈ columns of city (wrong) | data types |
| `ALTER TABLE city MODIFY (\|` (Oracle) | ≈ columns of city | ✓ columns (then types after the name) |
| `ADD CONSTRAINT fk FOREIGN KEY (\|`, `ADD PRIMARY KEY (\|` | ≈ columns of city | ✓ (keep) |
| `… FOREIGN KEY (CountryCode) REFERENCES \|` | kw | tables, the likely target first (FK guess from the column name) |
| `… REFERENCES country (\|` | ✗ columns of **city** | columns of `country` |
| `ADD CONSTRAINT ck CHECK (\|` | ≈ columns (no functions/keywords) | columns + functions |
| `ALTER TABLE city DROP CONSTRAINT \|` | kw | constraint names of `city` (needs metadata) |
| `ALTER TABLE city RENAME \|` / `RENAME TO \|` | kw | `COLUMN`, `TO`, `CONSTRAINT` / nothing |
| `DROP TABLE \|`, `DROP TABLE IF EXISTS \|`, `DROP TABLE city, \|` | kw | tables only |
| `DROP VIEW \|`, `DROP MATERIALIZED VIEW \|` | kw | views / materialized views only |
| `DROP TABLE world.\|` | ✗ empty | tables of `world` |
| `DROP INDEX \|`, `DROP SEQUENCE \|`, `DROP FUNCTION \|`, `DROP TRIGGER \|` | kw | index/sequence/function/trigger names (functions are cached; others need metadata) |
| `… CASCADE`/`RESTRICT`/`PURGE` after the names | kw | those keywords |
| `TRUNCATE \|` | ✓ tables + schemas | — |
| `TRUNCATE TABLE \|` | kw | tables |
| `CREATE VIEW v AS SELECT …`, `CREATE MATERIALIZED VIEW …` | ✓ | — |
| `CREATE VIEW v (\|) AS …` | ≈ columns of the body's tables | nothing (new names) |
| `CREATE INDEX i ON \|` | ✗ empty | tables (and matviews) |
| `CREATE INDEX i ON city (\|`, `USING btree (\|` | ✗ empty | columns of `city` |
| `CREATE INDEX i ON city(\|` | ✗ tables | columns of `city` |
| `CREATE UNIQUE INDEX … WHERE \|` | ≈ tables only | columns of `city` + functions |
| `CREATE TRIGGER trg AFTER INSERT ON \|` | ✗ empty | tables/views |
| `COMMENT ON TABLE \|` / `COLUMN \|` | kw | tables / tables then `t.` |
| `COMMENT ON COLUMN city.\|` | ✗ empty | columns of `city` |
| `GRANT SELECT ON \|` | ✗ empty | tables/views (roles after `TO`: out of scope) |

**Completion — DML tails** (INSERT/UPDATE/DELETE/MERGE work otherwise)

| Position | Today | Expected |
| --- | --- | --- |
| `INSERT … VALUES (…) \|` | kw | `RETURNING`, `ON CONFLICT` (PG/SQLite), `,` rows |
| `INSERT/UPDATE/DELETE … RETURNING \|`, `RETURNING ID, \|` | kw | columns of the target table (+ `*`); Oracle: then `INTO` |
| `ON CONFLICT (\|` | ≈ columns (via "any paren") | ✓ columns of the target |
| `ON CONFLICT (Name) DO \|` | kw | `UPDATE SET`, `NOTHING` |
| `ON CONFLICT … DO UPDATE SET \|` | ≈ columns + all tables | columns of the target, `EXCLUDED.` columns |
| `MERGE INTO \|`, `MERGE INTO city c USING \|` | kw | tables/views |
| `MERGE … WHEN MATCHED THEN UPDATE SET \|` | ≈ columns + aliases + tables | target's columns |

**Other features in DDL**

- Hover: works where the position checker happens to classify (`CREATE INDEX i ON city`,
  `REFERENCES country`, the SELECT of `CREATE VIEW`); nothing on the table in `DROP TABLE`,
  `ALTER TABLE`, `TRUNCATE TABLE`, `COMMENT ON TABLE`, nor on columns in `DROP COLUMN`,
  index column lists, `RETURNING`.
- Go to definition of a view: ✓ anywhere (looked up by name), including `DROP VIEW v`.
- Diagnostics: only statements starting with `SELECT/WITH/INSERT/UPDATE/DELETE/MERGE/VALUES`
  are checked → nothing for DDL: unknown table in `ALTER/DROP/CREATE INDEX/REFERENCES/COMMENT`,
  unknown column in `DROP/RENAME/ALTER COLUMN`, index lists, `REFERENCES t (col)`; and the
  SELECT inside `CREATE VIEW … AS` / `CREATE TABLE … AS` is never linted. (Tables created or
  altered in the document are already known to the linter.)
- Completion after DDL in the same file: `CREATE TABLE t (id int); SELECT t.| FROM t` →
  nothing until the statement is run and `sqlh.refresh` is done by hand.
- Formatter (verified, so never corrupts — the layout is the problem):
  `numeric(12,2)` → split over three lines; `CHECK (` → `CHECK(` with the IN list exploded;
  `CREATE TABLE orders (` / `ON orders (col)` glued to `orders(`; `CREATE INDEX … ON` breaks
  before `ON`; `CREATE OR REPLACE VIEW` → `CREATE` / `OR REPLACE VIEW` (OR read as a
  condition) and the view's `SELECT` stays on the `AS` line; `ALTER`/`DROP` untouched (fine).

#### 9.2 Design

- **Token-based DDL context, not the sqls AST.** The AST's paren/function/`ON` handling is
  the cause of most failures above; the linter already shows a token walk is reliable here.
  New `internal/completer/ddl.go`: `ddlContext(stmtTokens, cursor, dialect)` walks the tokens
  of the statement at the cursor (sqlsplit + lexer, comments/whitespace dropped, unterminated
  input tolerated) and returns `(*CompletionContext, ok)`. `Complete()` asks it first; `ok =
  false` → the existing path (so SELECT/INSERT/UPDATE/DELETE behaviour is unchanged).
  Handles: statements starting with `CREATE`/`ALTER`/`DROP`/`TRUNCATE`/`COMMENT`/`GRANT`/
  `REVOKE`/`RENAME`, and the DML tails (`RETURNING`, `ON CONFLICT`, `MERGE INTO/USING`).
  CREATE … AS SELECT / CREATE VIEW bodies are handed back to the existing path (they work).
- **State tracked by the walk**: statement kind and object kind (`TABLE`/`VIEW`/
  `MATERIALIZED VIEW`/`INDEX`/…), target table (`ALTER TABLE x`, `CREATE INDEX … ON x`,
  `COMMENT ON COLUMN x.`, DML target), a paren stack with a role per level (column
  definitions, column-name list of table X, type arguments, CHECK/WHERE expression,
  `REFERENCES` target list), columns defined so far in `CREATE TABLE`, and the last 1–3
  significant words.
- **New completion types**: `DataType`; `ContextKeyword` (a position's own keyword set,
  multi-word items like `PRIMARY KEY`, `NOT NULL`, `IF EXISTS`); table candidates filtered by
  kind (table / view / matview; before the background load knows view kinds, all relations);
  `ColumnsOf(table)` (explicit table instead of the scope); `DefinedColumns` (columns of the
  `CREATE TABLE` being written); `None` (new-name positions return an empty list rather than
  keyword noise).
- **Data types**: curated per dialect in `dialect/types.go` (PG, Oracle, SQLite; generic
  ANSI list for the rest), inserted as plain words (no snippets, see decisions); the
  parameter form shown in the detail (`varchar(n)`, `numeric(p, s)`, `VARCHAR2(n [CHAR])`).
  PG user types (enums, domains, composites from `pg_type` of the allowed schemas) and Oracle
  object types (`ALL_TYPES`) via the background load (M8).
- **Ordering**: in `REFERENCES |` the table guessed from the FK column name (`customer_id` →
  `customer`, reuse the relgraph guess) first; in `REFERENCES t (|` PK/unique columns first.
- **Hover** uses the same context: the table/column under the cursor in DDL positions.
- **Lint** gets a DDL pass with the same token walk (same "quiet when unsure" rules): unknown
  table in `ALTER TABLE`/`DROP TABLE|VIEW|MATERIALIZED VIEW` (not with `IF EXISTS`)/`TRUNCATE`/
  `CREATE INDEX ON`/`REFERENCES`/`COMMENT ON`/`GRANT … ON`; wrong object kind (`DROP VIEW`
  on a table, `DROP TABLE` on a view, `DROP VIEW` on a materialized view: the database refuses
  these); unknown column in `ALTER TABLE … DROP|RENAME|ALTER|MODIFY COLUMN`, index column
  lists, `PRIMARY KEY|UNIQUE|FOREIGN KEY (…)` of `ALTER TABLE`, `REFERENCES t (cols)`,
  `COMMENT ON COLUMN`; `ADD COLUMN` of a column that exists; unknown index/constraint/
  sequence/trigger in `DROP` (no `IF EXISTS`; needs M8); in `CREATE TABLE`, `PRIMARY KEY|
  UNIQUE|FOREIGN KEY (…)` naming a column the statement doesn't define. The `AS SELECT` body
  of `CREATE VIEW/MATERIALIZED VIEW/TABLE` is linted like a SELECT. Schemas outside the cache
  stay unchecked.
  - *Document exemptions, refined for DDL*: today any table created or altered **anywhere**
    in the document is exempt and an altered table's columns go unchecked — that would hide
    `ALTER TABLE city DROP COLUMN nocol` (city is altered by that very statement). For the DDL
    rules: only objects created **earlier** in the document are exempt, and only the columns
    added/renamed by earlier statements. A second `DROP` of an object dropped earlier in the
    document stays quiet.
  - *Statements already run*: with refresh after DDL, a statement turns red once it has run
    (`DROP TABLE x` → unknown table, `ADD COLUMN y` → already exists, `CREATE TABLE`
    unaffected). The server remembers the DDL statements it executed successfully this session
    (normalised text, per connection + database, like result reuse) and skips DDL rules for
    them; editing the statement brings the checks back.
- **Refresh after DDL**: when a run (editor, CLI via the server, dump) executed at least one
  DDL statement successfully (`CREATE`/`ALTER`/`DROP`/`RENAME`/`COMMENT`, PL/SQL blocks with
  `EXECUTE IMMEDIATE '<DDL>'`), the server refreshes the schema cache once the job ends, also
  when a later statement of the same run failed. Config `refreshAfterDDL` (default `true`);
  `sqlh.refresh` / `space i U` / `sqlh refresh` stay. The summary says so (`… · schema
  refreshed`). Standalone CLI runs have no cache: nothing to do.

#### 9.3 Steps

- [x] Census as permanent tests: `internal/ddl/ddl_test.go` (slots per position and dialect,
      refs, 224 cases), `internal/handler/ddl_completion_test.go` (one case per census row
      against the mock DB, ranking, lower-case keywords), `ddl_hover_test.go`
- [x] M1/M2/M3/M4 — `internal/ddl`: token walk (`Tokenize`, `Analyze`, `AtCursor`,
      `InStatement`); a cursor marker after the tokens of the statement being typed (text up to
      the cursor, the word being typed removed; `sqlsplit.Statement.Terminated` tells a finished
      statement). Grammar in `grammar.go` (CREATE TABLE/VIEW/INDEX/TRIGGER/SEQUENCE/SYNONYM/
      TYPE/SCHEMA, ALTER TABLE/VIEW/INDEX/SEQUENCE/TRIGGER/TYPE, DROP, TRUNCATE, COMMENT,
      GRANT/REVOKE, Oracle RENAME) and `dml.go` (INSERT/UPDATE/DELETE tails, ON CONFLICT,
      MERGE, WITH … DML); everything else hands off to the regular completer (`Handoff`).
      A `stop` panic ends the walk at the cursor / the end / anything unexpected (`Unsure`).
      `Slot`: object kinds (relations filtered by kind once views are loaded), qualifier,
      columns of a named table, columns the statement defines, types, functions, EXCLUDED,
      keywords, FK guess, keys first. Names glued with `$`/`#` (Oracle V$…, V500$ACT_GE) put
      back together (the lexer splits them). Completer: `internal/completer/ddl.go`, asked
      first in `Complete()`; filters by the word being typed incl. `$`/`#`.
- [x] Data types: `dialect/types.go` (PostgreSQL 41, Oracle 27, SQLite 16, generic 22, with
      parameters and a description), plus the database's user types; plain words
- [x] M5 — keywords per position and dialect (`internal/ddl/keywords.go`, `"WORDS@posg"` tags);
      Oracle keyword list completed (`oracleExtraKeywords`: JOIN, MERGE, PRIMARY, KEY, …)
- [x] M6 — hover (`internal/handler/ddl_hover.go`, before the regular hover: tables, columns,
      indexes, constraints, sequences, triggers in DDL and DML tails); lint DDL pass
      (`internal/lint/ddl.go`): document state in order (created/dropped tables, columns
      added/dropped/renamed, objects), also from `EXECUTE IMMEDIATE '<DDL>'` in skipped PL/SQL /
      DO blocks; `AS SELECT` bodies linted as queries (`bodyStatement`); DML tails' columns.
      Catalog: relation kinds (default schema, once views are loaded), objects with partial
      lists; unqualified objects only sure when the current schema is cached (Oracle allow-list)
- [x] M7 — `refreshAfterDDL` (default on): `exec.IsDDL` (CREATE/ALTER/DROP/RENAME/COMMENT, also
      dynamic DDL in PL/SQL / DO blocks), refresh at the end of the job with a repository
      captured at start (no server lock: `stopJob` holds it while waiting for the job),
      `· schema refreshed` in the summary, also after a later statement failed; executed DDL
      remembered per connection + database (`jobState.executed`, normalised text) and skipped
      by the lint (`lint.Options.SkipDDL`), re-lint after the run
- [x] M8 — `database.SchemaObjects` / `ObjectLister` (background load): PostgreSQL (`pg_index`,
      `pg_constraint` incl. PG 18 not-null constraints marked generated, `pg_class` sequences,
      `pg_trigger`, `pg_type` enum/domain/composite/range; 351 objects in 30 ms on pg_local),
      Oracle (flat queries joined in Go: 66 s → 3 s on CADE as `cade_read_only`; system-named
      CHECK constraints left out — telling NOT NULL apart cost 4.5 s; triggers and sequences of
      another owner marked partial: `ALL_TRIGGERS`/`ALL_SEQUENCES` don't show them all),
      SQLite (indexes with columns, triggers). Completion, hover, lint use them.
- [x] M9 — formatter: `internal/formatter/ddl.go`, applied after the sqls formatter to the
      DDL statements (paired by sqlsplit): token-based layout from the whitespace-collapsed
      source (adjacency kept: `t (` stays `t (`, `users(id)` stays), keywords cased like the
      formatter does plus DDL words and type names, names the statement uses kept as written;
      CREATE TABLE elements one per line, ALTER TABLE with several actions one per line,
      `CREATE VIEW/TABLE … AS` header + the query formatted on its own, the rest one line;
      statements with comments left to the sqls formatter. Still verified (tokens, fixed
      point). Goldens updated (`create INDEX x\non t(a)` → `create index x on t (a)`).
- [x] README (completion checklist, DDL diagnostics, formatting, `refreshAfterDDL`),
      `doc/config.example.yml`, `doc/cli.md`
- Verified: unit tests (+ `-race`); lint on the chemalert Oracle scripts (1159 files, 40.7k
  statements, also pretending to log in as CADE): no parse gaps left after the `$` fix,
  findings are real (migrations already applied: "already has a column", objects of the DE
  schema or of other versions), two false-positive classes found and fixed (dynamic DDL in
  PL/SQL blocks, `GRANT … ON` a sequence/package → refs marked `Soft`); 478 KB script linted
  in 75 ms; live completion on pg_local and Oracle (CADE via `cade.`); end to end over stdio
  (`/tmp/sqlh-e2e/e2e.py`: complete, hover, diagnostics, run DDL → refreshed cache, executed
  statements not flagged); formatter battery: 18 formatted, 0 corrupted.
- Known limits: Oracle logins other than the owner see unqualified names of the login schema
  only (as for queries; `cade.` works); `CREATE FUNCTION/PROCEDURE/PACKAGE` bodies, roles
  after `GRANT … TO`, `PARTITION` clauses are not followed (regular completer / quiet lint).

#### 9.4 Decisions (2026-10-14)

1. **Dialects**: PostgreSQL, Oracle, SQLite get specific keywords/types/metadata; the other
   drivers the generic ANSI set, expanded only if another database is needed.
2. **Keywords**: where the position is understood, only its keywords — also with a prefix
   typed. The whole list only where the position isn't understood.
3. **Refresh after DDL**: automatic, `refreshAfterDDL: true` by default; the refresh keybinding
   and command stay.
4. **No document overlay**: tables created in the file aren't known to completion before
   they're run (not a workflow here). The linter keeps tolerating them.
5. **Metadata** for indexes, constraints, sequences, triggers, user types: now (M8).
6. **DDL diagnostics**: yes, including unknown tables in `DROP` without `IF EXISTS` (errors
   should show before running).
7. **Formatter**: part of this phase (M9).
8. **Snippets**: plain words only for now. Candidate for a later phase (see Phase 5).

### Phase 5 — later / optional

- [ ] `EXPLAIN` to buffer
- [ ] Snippets/templates (after Phase 9, if wanted): FK-guessed `REFERENCES customer (id)` for
      a column named `customer_id`; type parameters with tab stops (`varchar(|)`); clause
      skeletons (`ADD CONSTRAINT name FOREIGN KEY (…) REFERENCES t (…)`, `ON CONFLICT (…) DO
      UPDATE SET …`, `CREATE INDEX name ON t (…)`) with the names filled from the context
- [ ] Server-side LIMIT rewriting for simple SELECTs (PostgreSQL done with a cursor; MySQL
      drains like pgx if ever needed)
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

- **2026-10-14** PostgreSQL previews through a server-side cursor: `space i r` on a large
  query no longer fetches the whole result (5M rows: 2.8 s → 3 ms).
  Fix: each preview declares its own cursor (`sqlh_preview_N`). One shared name made every
  `FETCH` the same text, which pgx's statement cache served with the first preview's result
  columns: the next query with another column count failed (08P01 "bind message has 5 result
  formats but query has 9 columns") for the rest of the session, and after a 1-column first
  query results were decoded with that column's type.

- **2026-10-14** Phase 8: connection header in query files (kept up to date by the editor),
  `sqlh dump` for running query files from the terminal, `space i d` with result reuse.

- **2026-10-14** TOML config dropped: YAML stays the only config format.

- **2026-10-14** Phase 9 planned: DDL census (~100 positions probed; completion after the
  first keywords of CREATE/ALTER/DROP/COMMENT/INDEX and DML `RETURNING`/`ON CONFLICT` tails
  broken, hover partly, no DDL lint, formatter layout issues), token-based design, steps,
  open questions.

- **2026-10-14** Phase 9 done: `internal/ddl` (token walk for DDL and DML tails) behind
  completion, hover and a DDL lint pass; index/constraint/sequence/trigger/type metadata;
  schema cache reloaded after DDL runs (`refreshAfterDDL`); DDL formatting layout.

## 6. Environment notes

- Helix: `[language-server.sqlh] command = "sqlh"`, `args = ["-log", "/tmp/sqlh.log"]`
- Binary: `$GOPATH/bin/sqlh` (`just install` from repo root); CLI: `doc/cli.md`
- Tests isolating sockets/state: `SQLH_RUNTIME_DIR=/tmp/sqlh-rt XDG_STATE_HOME=/tmp/sqlh-state`;
  `/tmp/sqls-build/t_p3.py` (CLI↔server scenario)
- PostgreSQL exec tests (cursor previews) run against a real server, skipped otherwise:
  `SQLH_TEST_PG='host=localhost user=… password=… dbname=sdsai sslmode=disable' go test ./internal/exec`
- Config: `~/.config/sqlh/config.yml` (example: `doc/config.example.yml`); connections
  `oracle_dev` (default; keychain `sqlh-oracle_dev`, schemas CADE), `pg_local`, `sqlite_scratch`
- Test files: `scratch/test.sql` (PG), `scratch/oracle.sql`, `scratch/sqlite.sql`
- SQLite scratch DB: `/tmp/sqls-build/scratch.db` (customer/orders/big_orders)
- Local PG: Docker `llm-sds-reader-postgres-1`, `admin`/`verysecret`, db `sdsai`, schema `cadetst`
- Oracle dev: see `connection.toml` (gitignored); port 1521 reachable
- Protocol test client: `/tmp/sqls-build/lspc.py` (+ `t_*.py` scenario scripts;
  `t_fmtsafety2.py` = formatter battery); Oracle
  repo probe: `scratch/oradbg/` (reads creds from env `H S U P`)
- Schema objects (indexes, constraints, …): `go run ./scratch/objprobe ALIAS [NAME]`;
  lint/completion against a live cache: `SQLH_LINTPROBE=alias:dir` / `SQLH_COMPLPROBE=alias
  CASES='DROP INDEX |;;…'` `go test ./internal/handler -run 'TestLintProbe|TestComplProbe' -v`
  (`internal/handler/live_probe_test.go`; `PROBE_SCHEMA=CADE` pretends to log in as CADE);
  chemalert Oracle scripts: `~/work/chemalert/chemalert/{src/database/oracle,db/dcrs}`
- Relationships: `CFG=<config> go run ./scratch/relg ALIAS [TABLE…]` prints the graph
  (`MAYBE=1`: all guesses); PG test schema with views: `scratch/relations_pg.sql`;
  views and their writability: `go run ./scratch/viewprobe ALIAS [all]`; pg_local test views
  (cadetst): `scratch/views_pg.sql`;
  scenario scripts `/tmp/sqls-build/t_rel*.py` (`t_rel_pty.py` drives real fzf in a pty)
