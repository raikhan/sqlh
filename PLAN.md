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
| Pickers / UI               | (2026-10-16, Phase 10) Own Bubble Tea TUI replaces fzf everywhere: a side panel tied to a Helix pane (Herdr), an inline popup for one-shot picks, and `sqlh tui` as a standalone app.                       |

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
function completion (`DataBaseFunctions` returned `[]` for PG/SQLite and Oracle's _reserved
words_), and both failed on CTE columns. A wider probe found more subquery bugs.

- [x] Scope resolution rewrite (`parser/parseutil/scope.go`): query levels = statement +
      enclosing subqueries; each level sees its own `FROM`/`JOIN` (not those of nested
      subqueries, CTE bodies, function arguments like `extract(year from …)`), the branch of a
      `UNION`/`INTERSECT`/`EXCEPT`/`MINUS` containing the cursor, and the enclosing levels'
      tables as `Outer` (qualified use only). Replaces the sqls heuristics (first `FROM` of the
      whole statement; subqueries _after_ the cursor), which leaked a CTE body's tables into the
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

### Phase 7 — relationships between tables (DBeaver-like explorer, in fzf) ✅ done

Decided: no separate TUI (superseded 2026-10-16 by Phase 10); the table picker and `describe` cover it. Schemas are boundaries
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
- [x] Lineage for `UNION` views (2026-10-16): a set operation's column has one origin per
      branch adding rows (`SubQueryColumn.Origins`, flattened through nested unions, derived
      tables, CTEs, views on views; `EXCEPT`/`MINUS` right sides add none). The same column in
      every branch: full lineage. Different columns: an edge to a table all of them reference
      (multi-column too, a branch's columns from one table reference), "via N tables"
      (`Edge.ViaTables`), outgoing only, "maybe" if a branch only has a guess. Hover lists a
      view column's origins (`RelGraph.ColumnOrigins`, capped at 5). CADE: +1573 edges, 135
      UNION views, e.g. `CACLIENTMODULEUSAGE → CABUSINESSUNIT via 12 tables`.
      - Fix first (f02b100): a UNION in a derived table/CTE took the first branch as the
      origin (9 wrong CADE edges); columns matched by position, unnamed expressions keep
      theirs (CTE column lists were shifted); Oracle `MINUS` (an identifier for the lexer)
      was swallowed as a table alias.
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
- [x] Go to definition of a table (2026-10-16), the same way as views, fetched without the
      server lock (5 s): Oracle `DBMS_METADATA.GET_DDL` (2 s budget: 15 s cold on CADE2, then
      0.4 s) when allowed, else — ORA-31603 for `cade_read_only`, remembered — generated from
      `ALL_*` like DBeaver does (DBeaver silently falls back too); PostgreSQL from
      `pg_catalog`; SQLite `sqlite_master`; failing that from the schema cache (marked
      approximate). Targets: names, `schema.name` (allowed schemas; views of other schemas
      fetched), DDL statements (`internal/ddl` refs), columns (cursor on their line), Oracle
      synonyms (`ALL_SYNONYMS`). `sqlh ddl TABLE` prints it (`control` `sqlh/ddl`, standalone).

### Phase 8 — query files: connection header, `sqlh dump`, result reuse

Goal: query files run from the terminal (automation) on the connection last chosen in Helix,
CSV written next to the caller (`<file>.csv`); `space i d` does the same from the editor.

Decided:

- **Header** `-- sqlh: conn=ALIAS [db=NAME]` in the file's leading comments (`key=value`,
  other keys kept). `db=` only when the database was switched (`space i D`). No schema: it
  comes from the connection (DSN `search_path`, Oracle login schema); session state
  (`SET search_path`, `ALTER SESSION`) is not carried — put it in the file.
  _Superseded 2026-10-16_: `schema=` when the schema was switched (see "Current schema").
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

| Position                                                                                  | Today                             | Expected                                                                                                                                                           |
| ----------------------------------------------------------------------------------------- | --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `CREATE TABLE \|`, `… IF NOT EXISTS \|`                                                   | kw                                | nothing / `IF NOT EXISTS`, schema names for `schema.`                                                                                                              |
| `CREATE TABLE t (\|` and `(id int, \|`                                                    | ✗ empty                           | column name: nothing; table-constraint keywords (`CONSTRAINT`, `PRIMARY KEY`, `FOREIGN KEY`, `UNIQUE`, `CHECK`, PG `LIKE`, `EXCLUDE`)                              |
| `CREATE TABLE t (id \|`, `(id va\|`                                                       | ✗ empty                           | data types of the dialect                                                                                                                                          |
| `CREATE TABLE t (id int \|`                                                               | ✗ empty                           | column-constraint keywords (`NOT NULL`, `NULL`, `DEFAULT`, `PRIMARY KEY`, `UNIQUE`, `REFERENCES`, `CHECK`, `GENERATED … AS IDENTITY`, `COLLATE`, Oracle `ENABLE`…) |
| `… DEFAULT \|`                                                                            | ✗ empty                           | functions/literals (`now()`, `CURRENT_TIMESTAMP`, `SYSDATE`, `gen_random_uuid()`, `nextval(`)                                                                      |
| `… REFERENCES \|` (column or table constraint)                                            | ✗ empty                           | tables                                                                                                                                                             |
| `… REFERENCES country (\|`, `country(\|`                                                  | ✗ empty                           | columns of `country` (PK/unique first)                                                                                                                             |
| `PRIMARY KEY (\|`, `UNIQUE (\|`, `FOREIGN KEY (\|` in CREATE TABLE                        | ✗ empty                           | columns defined earlier in the statement                                                                                                                           |
| `CHECK (\|` in CREATE TABLE                                                               | ✗ empty                           | columns defined in the statement, functions                                                                                                                        |
| `CREATE TABLE t (LIKE \|` (PG)                                                            | ✗ empty                           | tables                                                                                                                                                             |
| `CREATE TABLE t AS SELECT …`                                                              | ✓                                 | —                                                                                                                                                                  |
| `ALTER TABLE \|`, `ALTER TABLE ci\|`                                                      | kw / empty                        | tables (views for `ALTER VIEW`, matviews for `ALTER MATERIALIZED VIEW`)                                                                                            |
| `ALTER TABLE world.\|`                                                                    | ✗ empty                           | tables of `world`                                                                                                                                                  |
| `ALTER TABLE city \|`                                                                     | kw                                | `ADD`, `DROP`, `ALTER`, `RENAME`, PG `OWNER TO`/`SET`/`ENABLE`…, Oracle `MODIFY`                                                                                   |
| `… DROP COLUMN \|`, `DROP \|` (MySQL/Oracle style), `RENAME COLUMN \|`, `ALTER COLUMN \|` | kw                                | columns of `city` (+ `COLUMN`/`CONSTRAINT` after a bare `DROP`)                                                                                                    |
| `… ALTER COLUMN Name \|`                                                                  | kw                                | `TYPE`, `SET`, `DROP` (`DEFAULT`, `NOT NULL`)                                                                                                                      |
| `… ALTER COLUMN Name TYPE \|`, `ADD COLUMN x \|`, `ADD x \|`                              | kw                                | data types                                                                                                                                                         |
| `ALTER TABLE city ADD (x \|` (Oracle)                                                     | ≈ columns of city (wrong)         | data types                                                                                                                                                         |
| `ALTER TABLE city MODIFY (\|` (Oracle)                                                    | ≈ columns of city                 | ✓ columns (then types after the name)                                                                                                                              |
| `ADD CONSTRAINT fk FOREIGN KEY (\|`, `ADD PRIMARY KEY (\|`                                | ≈ columns of city                 | ✓ (keep)                                                                                                                                                           |
| `… FOREIGN KEY (CountryCode) REFERENCES \|`                                               | kw                                | tables, the likely target first (FK guess from the column name)                                                                                                    |
| `… REFERENCES country (\|`                                                                | ✗ columns of **city**             | columns of `country`                                                                                                                                               |
| `ADD CONSTRAINT ck CHECK (\|`                                                             | ≈ columns (no functions/keywords) | columns + functions                                                                                                                                                |
| `ALTER TABLE city DROP CONSTRAINT \|`                                                     | kw                                | constraint names of `city` (needs metadata)                                                                                                                        |
| `ALTER TABLE city RENAME \|` / `RENAME TO \|`                                             | kw                                | `COLUMN`, `TO`, `CONSTRAINT` / nothing                                                                                                                             |
| `DROP TABLE \|`, `DROP TABLE IF EXISTS \|`, `DROP TABLE city, \|`                         | kw                                | tables only                                                                                                                                                        |
| `DROP VIEW \|`, `DROP MATERIALIZED VIEW \|`                                               | kw                                | views / materialized views only                                                                                                                                    |
| `DROP TABLE world.\|`                                                                     | ✗ empty                           | tables of `world`                                                                                                                                                  |
| `DROP INDEX \|`, `DROP SEQUENCE \|`, `DROP FUNCTION \|`, `DROP TRIGGER \|`                | kw                                | index/sequence/function/trigger names (functions are cached; others need metadata)                                                                                 |
| `… CASCADE`/`RESTRICT`/`PURGE` after the names                                            | kw                                | those keywords                                                                                                                                                     |
| `TRUNCATE \|`                                                                             | ✓ tables + schemas                | —                                                                                                                                                                  |
| `TRUNCATE TABLE \|`                                                                       | kw                                | tables                                                                                                                                                             |
| `CREATE VIEW v AS SELECT …`, `CREATE MATERIALIZED VIEW …`                                 | ✓                                 | —                                                                                                                                                                  |
| `CREATE VIEW v (\|) AS …`                                                                 | ≈ columns of the body's tables    | nothing (new names)                                                                                                                                                |
| `CREATE INDEX i ON \|`                                                                    | ✗ empty                           | tables (and matviews)                                                                                                                                              |
| `CREATE INDEX i ON city (\|`, `USING btree (\|`                                           | ✗ empty                           | columns of `city`                                                                                                                                                  |
| `CREATE INDEX i ON city(\|`                                                               | ✗ tables                          | columns of `city`                                                                                                                                                  |
| `CREATE UNIQUE INDEX … WHERE \|`                                                          | ≈ tables only                     | columns of `city` + functions                                                                                                                                      |
| `CREATE TRIGGER trg AFTER INSERT ON \|`                                                   | ✗ empty                           | tables/views                                                                                                                                                       |
| `COMMENT ON TABLE \|` / `COLUMN \|`                                                       | kw                                | tables / tables then `t.`                                                                                                                                          |
| `COMMENT ON COLUMN city.\|`                                                               | ✗ empty                           | columns of `city`                                                                                                                                                  |
| `GRANT SELECT ON \|`                                                                      | ✗ empty                           | tables/views (roles after `TO`: out of scope)                                                                                                                      |

**Completion — DML tails** (INSERT/UPDATE/DELETE/MERGE work otherwise)

| Position                                                  | Today                        | Expected                                                 |
| --------------------------------------------------------- | ---------------------------- | -------------------------------------------------------- |
| `INSERT … VALUES (…) \|`                                  | kw                           | `RETURNING`, `ON CONFLICT` (PG/SQLite), `,` rows         |
| `INSERT/UPDATE/DELETE … RETURNING \|`, `RETURNING ID, \|` | kw                           | columns of the target table (+ `*`); Oracle: then `INTO` |
| `ON CONFLICT (\|`                                         | ≈ columns (via "any paren")  | ✓ columns of the target                                  |
| `ON CONFLICT (Name) DO \|`                                | kw                           | `UPDATE SET`, `NOTHING`                                  |
| `ON CONFLICT … DO UPDATE SET \|`                          | ≈ columns + all tables       | columns of the target, `EXCLUDED.` columns               |
| `MERGE INTO \|`, `MERGE INTO city c USING \|`             | kw                           | tables/views                                             |
| `MERGE … WHEN MATCHED THEN UPDATE SET \|`                 | ≈ columns + aliases + tables | target's columns                                         |

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
  - _Document exemptions, refined for DDL_: today any table created or altered **anywhere**
    in the document is exempt and an altered table's columns go unchecked — that would hide
    `ALTER TABLE city DROP COLUMN nocol` (city is altered by that very statement). For the DDL
    rules: only objects created **earlier** in the document are exempt, and only the columns
    added/renamed by earlier statements. A second `DROP` of an object dropped earlier in the
    document stays quiet.
  - _Statements already run_: with refresh after DDL, a statement turns red once it has run
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
- [x] Follow-up (testing in Helix): no list right after `DROP VIEW` while `select * from`
      had one. Helix only auto-requests after 2 typed word characters (debounced, sent with
      the cursor position when it fires); after `drop` the new keyword menu stayed open while
      typing `view`, so nothing was requested after the space (`select * from` got lucky: `*`
      closed the menu). Fix: `" "` is a completion trigger character; space-triggered requests
      are answered only at name positions (DDL slots with names/types, FROM/JOIN/INTO/UPDATE
      tables), else `[]` (no popup). Verified by driving Helix 25.07 through a pty with
      `hx -v --log` and a workspace `.helix/languages.toml` pointing at the test build.
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

### Current schema per connection (2026-10-16) ✅ done

Trigger: with a read-only login (`cade_read_only`, `sdsai`) the current schema was the
login's, empty: FKs, views, relationships, view go to definition were all off for CADE (the
CADE checks only worked through `PROBE_SCHEMA`).

Decided: one schema at a time per connection; `schema:` else the first of `schemas:` else the
login's. Oracle `ALTER SESSION SET CURRENT_SCHEMA` on every pooled session (`initConnector`
around go-ora's connector); PostgreSQL `SET search_path TO <schema>, <rest of the original>`
after connecting (pgx `OptionAfterConnect`) — not a startup parameter: PgBouncer refuses it
before PostgreSQL 18 (prod `srepgcluster:5411`, Percona 17.7). Probed (`scratch/pgbprobe`):
PgBouncer in **session** mode (two clients keep their backends; PIDs reused across runs), so
the session `SET` is safe and `DISCARD ALL` resets it on release; transaction mode would need
`SET LOCAL` per transaction (not built). Switching: `sqlh schema [NAME]` (fzf), `space i S`
(status moved to `space i s`), LSP `switchSchema`; reconnects, rolled back on failure; not
remembered (the header's `schema=` carries it). `run/export/dump --schema` standalone.
**Superseded by Phase 11 (2026-10-16):** switching removed; a connection is one database and
schema.
Config: every connection validated; PostgreSQL/MySQL/Vertica `host` without `proto` → tcp.

### Phase 10 — sqlh panel: own TUI replacing fzf (2026-10-16) ✅ done (branch `tui-experiment`)

Trigger: comparison with [lazydb](https://github.com/yelog/lazydb) (Rust/ratatui DB workspace).
Taken from it: the object tree (lazy, kinds grouped, sticky ancestors), an action menu on the
selection + contextual key hints, search prefixes, bounded read-only data previews, a record
view, SQL history, per-connection environment (dev/test/prod). Not taken: data grid / WHERE bar
(VisiData), data and catalog editing, dashboards, users & roles, Redis, credential store,
update center, MCP. Model for the Herdr/Helix side: `~/.config/swiper` (Go + Bubble Tea in a
Herdr pane driving the originating Helix with `herdr pane send-keys`) and
`~/.config/helix/herdr.nu` (`send-helix-command`, `helix-pane`, `helix_origin` token).

#### 10.1 Decisions

1. **One TUI, three hosts**, same components (`internal/tui`):
   - **panel**: persistent side pane, tied to the Helix pane that opened it — exits when that
     pane closes or its `hx` process ends (Herdr check, like Swiper's `-helix-pid` validation).
     Placement `panel.placement: right | left | up | down` (default `right`): the Helix pane
     is split in half; Herdr only splits right/down, so left/up are a split + `pane.swap`;
   - **inline popup**: one-shot picks under `:insert-output`/`:pipe-to` (height mode on
     `/dev/tty`, no alternate screen, kitty keyboard protocol off around it, stdout without a
     trailing newline, Esc prints nothing — the current fzf contract); also the fallback
     outside Herdr;
   - **`sqlh tui`**: standalone app (tree + previews + history) in the current terminal. In
     Herdr it can spin out a Helix pane for a new or existing query file and then acts as that
     editor's panel; closing that Helix detaches the app (it keeps running), it doesn't exit.
     The new Helix pane goes on the opposite side, so the app ends up where `placement` says.
     Outside Herdr: browse only, Enter copies to the clipboard.
   - **back to the editor**: `panel.keys.backToEditor` (default `backspace`, not while typing
     a search) focuses the panel's Helix pane — useful when arriving from another pane (Herdr's
     `prefix space o` covers the previous-pane case).
2. **fzf goes away completely**: `tables -p`, `columns -p`, `connect`/`db`/`schema` without
   an argument, `run --limit ask`. The `-p` contract stays. `SQLH_FZF_ARGS --filter` (scripted
   use, tests) is replaced by a non-interactive selection mode (`SQLH_PICK=<query>`: pick the
   first match, print it, exit).
3. **Inserting into Helix** (Helix has no API and doesn't report the cursor): Enter in the
   panel stores a _pending action_ in the server (per panel, consumed once, expires after a
   few seconds), then sends the originating Helix `esc` + a key chord bound in Helix's config
   (default `space i P`, configurable `panel.insertKeys`) that runs
   `:insert-output sqlh panel take --editor -f r##'%{buffer_name}'## -l %{cursor_line} --col %{cursor_column}`.
   `take` computes the text _then_, from Helix's real cursor and live buffer (after `JOIN`:
   `customer c ON …`; a column: `alias.col` when its table is in the statement; else the name),
   so all of Phase 7's context logic is reused and shell quoting stays in Helix's config like
   the other `space i` bindings. One undo step, multiple selections work (Helix runs the
   command once). Bracketed-pasting text rejected: auto-pairs and mode-dependent behaviour.
   Afterwards the panel focuses Helix again. Pending actions are keyed by the Helix pane
   (`take` inherits Helix's `HERDR_PANE_ID`), so two editors in one workspace don't mix.
4. **Following the cursor**: the server records the last edit position per document (sync is
   full-text, so from the common prefix/suffix of old and new text) and positions from
   position-bearing requests (completion, hover, signature help, definition, code actions);
   the panel's "In this statement" section follows it. `space i t` syncs explicitly (passes
   file/line/col) and opens or focuses the panel; a plain Helix binding, remappable.
4a. **Herdr**: the CLI has no exact-pane focus and `pane split` takes no command, so sqlh
   talks to Herdr's socket API (`$HERDR_SOCKET_PATH`, newline-delimited JSON: `pane.split`,
   `pane.focus`, `pane.send_keys`, `pane.swap`, `pane.get`, `pane.process_info`). The panel is
   started through the shell startup hook convention of `~/.config/helix` (`HELIX_HELPER_ARGV`
   + `HELIX_ORIGIN_PANE_ID` in the new pane's environment: no echoed command, ownership
   token); if it hasn't registered with the server within 3 s, `exec <argv>` is typed into the
   new pane instead. The server keeps the registry of panels (their control connections).
5. **Previews from memory**: the panel loads the catalog snapshot and relationship graph once
   (and again on push notifications), so moving the cursor costs no process or round-trip.
6. **History = runs only**: every execution, versions grouped by similarity (10.6).
7. **Environment per connection**: `environment: dev | test | prod` (default `dev`, as
   lazydb; decided). Drives the sample policy (`dev`/`test`: automatic, `prod`: on demand only), a
   coloured label in the panel header and `sqlh status`; later the destructive-run
   confirmation (Phase 5).
8. **Stack**: Bubble Tea v1 + Lip Gloss + Bubbles (same versions as Swiper), a small fuzzy
   matcher (`sahilm/fuzzy` or own). Herdr calls adapted from Swiper's `herdr.go` into
   `internal/herdr` (checked calls, editor validation, serialized sends). Tests drive models
   directly and use a fake `herdr` CLI — never a live pane.

#### 10.2 Panel layout and keys (target)

```
 pg_local › sales  [dev]        following scratch/test.sql:42
▾ In this statement             tables at the cursor + their relations (→ ←), live
▾ pg_local › sales              Enter on a connection/database/schema switches
  ▾ Tables
    ▾ orders
      ▸ Columns   id · customer_id · status …   Space marks, Enter inserts "o.a, o.b"
      ▸ Relations → customer.id = customer_id    Enter: join clause; chain via nesting
      ▸ Keys / Indexes / Triggers
      ▸ Used by views
  ▸ Views  ▸ Materialized views  ▸ Functions / Packages  ▸ Sequences  ▸ Types
─ Describe │ DDL │ Sample │ History ─────────────────────────────
```

- `/` filters the loaded tree in memory (matches + their ancestors); `f` fuzzy search over
  every object, results shown under their path; prefixes `@` (connection/schema scope),
  `t:` `v:` `f:` `s:` … (kind).
- Action menu on the selection (Shift-Tab / `.`): insert name, join clause, join chain,
  columns, `SELECT … FROM` skeleton, copy, open DDL / go to definition (read-only file, as
  `gd`), refresh this object. Footer: the keys valid here.
- Sticky ancestors when scrolled; type icons (Nerd Font / Unicode / ASCII).

#### 10.3 Steps — foundations + panel core ✅ done (2026-10-16)

- [x] Dependencies (Bubble Tea v1.3.10, Lip Gloss v1.1.0, Bubbles v0.21.0, as Swiper);
      `internal/herdr`: socket API client (`pane.get/process_info/split/swap/focus/send_keys/
      send_text/close`, `NotFound`), `SQLH_NO_FOCUS=1` keeps the focus (scripted tests; passed
      on to the panel's pane).
- [x] Config: `environment` per connection (dev/test/prod; development/staging/production
      accepted; validated; unknown → prod by `Env()` for safety), `panel:` (`placement`,
      `insertKeys`, `keys.backToEditor`, `icons`), `tui.queriesDir` (`QueriesDirIn`). Status
      and connection lists carry the environment; `sqlh status` shows it. `sample:` → 10.5.
- [x] Control: `sqlh/catalog` (`catalog.Tree`: schemas → tables/views/matviews (kinds in the
      default schema once views are loaded), sequences, types, indexes/constraints/triggers
      with their table; SQLite's unnamed schema too), `sqlh/subscribe` + `sqlh/event`
      notifications (`cache`, `connection` with a generation counter, `cursor`, `focus`),
      `sqlh/panels`, `sqlh/panelFocus`, `sqlh/panelAction`/`sqlh/panelTake` (per editor pane,
      once, 15 s), panel settings in `Status.Panel`. Columns and relations stay per-table calls
      (cached in the panel; ~ms over the socket) rather than one big snapshot. Functions are
      not in the tree yet (PostgreSQL lists ~1000 built-ins without a schema to filter on).
- [x] Server: cursor from `didChange` (`editPosition`: common suffix first, then prefix) and
      from position-bearing requests (`trackCursor`), debounced 150 ms, only with subscribers.
- [x] `sqlh panel take` (`panelText`): text actions; columns qualified by the statement's
      alias (or name, with several tables); tables: copy when the cursor is on a table name,
      the chain from a statement table, a single join to a related statement table, else the
      name; `relations.insert`/`Mode`; `NeedSpace`.
- [x] `sqlh panel`: focus the registered panel of `$HERDR_PANE_ID` (cursor synced), else split
      (placement, half, left/up via swap) with `HELIX_HELPER_ARGV`/`HELIX_ORIGIN_PANE_ID`,
      wait 3 s for registration, else type `exec …`. `helixAncestor` pins the hx PID.
- [x] `internal/tui` panel: lazy tree with stable ids (expansion and column marks survive
      rebuilds), "In this statement" (statement tables + related ones), connection tree,
      Connections; relation nodes carry anchor + path (nested relations → chains); `/` fuzzy
      search over all objects (Enter inserts, Tab reveals); action menu (`.`/`a`/Shift-Tab);
      Describe/DDL preview tabs; contextual footer, `?` help; side-by-side layout ≥ 110
      columns; mouse wheel; keys arriving together ("jj") split. Not done: sticky ancestors.
- [x] Lifetime: a 1 s check (pane gone or hx PID gone → exit); reconnects to a restarted
      server (`:lsp-restart`), showing "server gone" meanwhile.
- [x] Docs: `doc/cli.md` (Panel section, pickers, Helix bindings: `t` → panel, `P` take, `T`
      the inline table picker), README, `doc/config.example.yml`. `~/.config/helix` is the
      user's to change (bindings below).
- [x] Verified live (Herdr + Helix 25.07 in a scratch tab, test build, SQLite with FKs, own
      config.toml/languages.toml): `space i t` opens the panel through the startup hook
      (ownership token set), again only syncs the cursor; typing updates "In this statement";
      Enter on a related table after `JOIN` → `join customer c ON c.id = o.customer_id`, also
      from insert mode; DDL tab; search via pasted text; quitting Helix closes the panel.
      Fixed on the way: `ReorderArgs` broke subcommands (`panel run`), "jj" arrived as one key.
      Not exercised live: multiple selections, two editors with a panel each (unit-tested
      registry), an `applyEdit` arriving while `take` runs.

#### 10.4 Steps — replace fzf ✅ done (2026-10-16)

- [x] `tui.Pick`: inline Bubble Tea on `/dev/tty` (no alternate screen, 80 % of the rows,
      kitty keyboard protocol pushed off/popped), fuzzy filter (DP scoring: word starts,
      runs, gaps), preview pane (async, cached per list generation), multi-select (Tab,
      Ctrl-A), `Expect` keys, `Bind` keys that reload the list (drill-down), `AcceptQuery`
      (typed numbers), `NoSort`; leaves nothing behind on exit.
- [x] Ported: `tables -p` (Ctrl-R/Ctrl-O/Ctrl-T, preview, copy mode), `relations -p`,
      `columns -p` (table first), `schemas -p`, `databases -p`, `connect`/`db`/`schema` without
      a name, `run --limit ask`. The panel's Connections and schema nodes switch too.
- [x] `SQLH_PICK=<query>` scripted mode; `relations_test.go`/`args_test.go` updated, picker
      unit tests; the fzf code, `_nav`, `SQLH_FZF_ARGS` and preview subprocesses are gone.
      Verified live: `:insert-output … tables -p` in Helix, Ctrl-R into customer, Enter on
      region → the two-line join chain.

#### 10.5 Steps — sample rows ✅ done (2026-10-16)

- [x] `sqlh/sample` (`handler/sample.go`): a pooled `*sql.Conn` of its own (not the pinned
      session; Oracle CURRENT_SCHEMA / PostgreSQL search_path come with every pooled
      session), 10 s timeout, `SELECT *` + dialect limit (`LIMIT`, Oracle `FETCH FIRST`, MSSQL
      `TOP`; limit+1 to tell truncation), default 50 rows (max 500), cells cut at 200
      characters; `Column`: `GROUP BY … ORDER BY 2 DESC`, 10 values. SQL built from names
      resolved in the cache (`catalog.Resolve`, `QuoteIdent`), never from the request's text.
      No read-only transaction: the statement is sqlh's own `SELECT`.
- [x] Panel cache per table / table+column; cleared on generation changes and on `run`
      events (emitted when a job ends).
- [x] Policy: connection `sample: auto | manual | off` (validated), default by environment
      (`dev`/`test` auto, `prod` manual); `CatalogResult.SamplePolicy`; the server refuses
      `off`. Auto samples tables once the selection rests 300 ms; views only on request.
- [x] Sample tab: per column the distinct values (`NULL×n`), `v` record view (`n`/`p`), `s`
      loads/refreshes (a column: most common values). Verified live (test environment).
      Not done: `o` (sample → CSV → viewer).

#### 10.6 Steps — history with versions ✅ done (2026-10-16)

- [x] `internal/history`: `Entry` (id, time, kind preview/export/dump/text/cli, file
      (symlinks resolved), line, SQL, conn/db/schema/env, ms, rows, affected, truncated,
      status ok/error/cancelled, error, result, lineage), `Append` (one JSON line, O_APPEND;
      creates `.sqlh/.gitignore`), `Load` (cached by size+mtime: the panel asks on every
      cursor move), `Find` (file, lineage, like-SQL, limit), `Versions` (distinct SQL per
      lineage). Lineage: the most similar latest version among the same file's queries in the
      last 2000 runs, token LCS ratio ≥ 0.6, else a new one. No pruning.
- [x] Recording: server `runJob` (every statement, also failures/cancels; `history` event),
      standalone runs and `sqlh dump` (`recordStandalone`).
- [x] `sqlh/history` (versions of the statement at a cursor, the workspace's recent runs),
      `sqlh/replaceStatement` (statement at the cursor → `workspace/applyEdit` with an
      unversioned `changes` edit, from a goroutine; the terminator kept), panel action
      `replace` handled by `panel take`.
- [x] Panel: History root (open when there are earlier versions): This statement (versions,
      `= current`), Recent runs; preview with the run's facts and a line diff; Enter replaces
      / inserts; `o` opens the result (viewer without the panel's terminal), `y` copies.
- [x] Verified live: two runs with an edit between → two versions, Enter on the older one
      replaced the statement in Helix. Fixed on the way: buffers were looked up by exact path,
      so `/tmp/…` (CLI) missed Helix's `/private/tmp/…` and read the file on disk
      (`samePath`).

#### 10.7 Steps — `sqlh tui` ✅ done (2026-10-16)

- [x] `sqlh tui [-C DIR]`: the panel model with `Standalone` (Query files root, `n`, `e`,
      header "editor: … / no editor"); `tui.Workspace` (QueryFiles, NewQuery, OpenFile,
      Editor) implemented by `tuiBackend` around the panel backend.
- [x] Headless server: without an editor's server the app runs `handler.RunHeadless`
      (control socket only; sidecar `headless: true`); an editor's server starting for the
      workspace asks it to `sqlh/release` (it stops entirely, closing its control
      connections so panels notice) and takes the socket; the app reconnects, and starts a
      headless one again when the editor's goes (after 1.5 s, so a starting editor wins).
- [x] `n`: `query_<N>.sql` in `tui.queriesDir` (`nextQueryNumber`: highest + 1, from 1,
      gaps not filled; `O_EXCL`), header `-- sqlh: conn=…`, `SELECT * FROM t` of the
      highlighted table; opened in `tui.editor` (default `hx`) in a split of the app's pane
      (opposite `placement`, via swap), through the startup hook or typed `exec`; the app
      re-subscribes as that pane's panel. Enter on a query file / `e` on a run: `:open` in
      the attached Helix, else a new pane. The attached Helix exiting detaches it.
- [x] Verified live: headless start, `n` with orders highlighted → `queries/query_1.sql`
      in a Helix pane, socket handed over (sidecar no longer headless), "In this statement"
      follows that Helix, a column inserted as `o.total`; Helix quit → "no editor", headless
      again; Enter on the file → new Helix, `space i t` there focuses the app (no second
      panel); quitting the app removes the socket. Fixed on the way: a stopped server kept
      its accepted control connections open.
      Not done: database/schema in the new file's header (only `conn=`), outside Herdr the
      app can't open files (it says so).
- [x] Fix after first use: the first frame was drawn at a guessed 60×30 before the terminal
      reported its size; in a shorter pane it scrolled the screen and left stale rows (tables
      over the header). Panel and picker now draw nothing until the size is known. `⟲`
      (drawn wide by the font, over the label) → `↺`.

#### 10.8 Decided (2026-10-16)

- Default `environment`: `dev`.
- `tui.queriesDir` (relative or absolute, default `queries`) and `query_<N>.sql` naming (10.7).
- The panel takes half of the split pane; `panel.placement` (default `right`).
- No toggle on `space i t` (a Helix binding); `panel.keys.backToEditor` (default `backspace`).

### Phase 11 — panel refinement after first use (2026-10-16) ✅ done (branch `tui-experiment`)

Feedback from the first real session (pg_local, `sqlh tui` + panel). Decisions in 11.9.

#### 11.0 Bugs ✅

- [x] **`sqlh tui` → new Helix pane showed a Nushell error.** `splitEditor` set only
      `HELIX_HELPER_ARGV`; `~/.config/helix/start-helper.nu` also reads
      `$env.HELIX_ORIGIN_PANE_ID`; Helix only opened through the 3 s typed-`exec` fallback.
      Now both are passed (origin = the app's pane); `TestSplitEditorEnv` (fake Herdr in
      `internal/herdr/herdrtest`, usable from any package).
- [x] **`sqlh tui` screen corrupted by log lines.** The in-process server's `log` output and
      jsonrpc2's own logger (a `log.New(os.Stderr…)` of its own: `SetLogger(log.Default())` on
      both ends of control connections now) wrote over the screen; `logAway` sends the log to
      `--log` / `{cache}/sqlh/tui.log` while the panel or the app runs. Verified live: the
      editor taking the socket over no longer prints `connection is closed` on screen.

#### 11.1 A connection is the target ✅

- [x] Removed: `sqlh db`, `sqlh schema`, `--schema` on run/export/dump/ddl/relations, the LSP
      commands `switchDatabase`/`switchSchema`, control `useDatabase`/`schemaChoices`/
      `useSchema`, the server's `curDBName`/`curSchema` and `targetKey`, the panel's `u`.
      `header.ErrSwitch`: `db=`/`schema=` in a header is an error diagnostic, refuses editor
      runs (`TestRunRefusesDBHeader`) and `dump`, and the header isn't restamped.
- [x] `DBConfig.Target()` (database, schema; from fields or the DSN: PostgreSQL key=value and
      URL `search_path`, Oracle login, SQLite file) and `Fields()` (labelled parts, passwords
      masked; `internal/database/fields.go`, tests); `group:`; alias uniqueness validated.
- [x] Connections tab: `alias  db › schema  env`, groups as folders (open when holding the
      current one); Describe as a form.
- [x] New / Edit / Duplicate / Delete: `internal/connedit` rewrites only the lines of the
      connection (yaml.v3 node of that item re-rendered at its indentation; comments, blank
      lines and the rest byte-for-byte; unchanged values keep their spelling), validated with
      `config.Parse` before an atomic write (permissions and symlinks kept). Form in the TUI
      (bubbles textinput; choices with ←/→; SQLite shows only the file); Ctrl-T tests
      (`database.Open` + ping). Servers re-read the config when its mtime changes
      (`reloadConfigIfChanged` on `connections`/`reloadConfig`; the current connection follows
      its alias and reconnects when its settings changed; a broken file is refused;
      `TestReloadConfig`); `Status.Panel.ConfigPath` says which file.

#### 11.2 Tabs ✅

- [x] 1 Connections · 2 Objects · 3 Statement · 4 Queries · 5 History (`1`–`9`, tab bar
      clickable, short labels when narrow); per-tab selection/scroll; default 3 in the panel, 2
      in `sqlh tui`. Queries in the panel too (the backend opens files with `:open`).
- [x] Keymaps: `panel.keys.<action>` / `picker.keys.<action>` (a key or a list), defaults in
      `config.DefaultPanelKeys`/`DefaultPickerKeys`, scopes (main, search, form; picker)
      validated for unknown actions and clashes; footer and `?` help from the keymap. The CLI
      picker reads `picker.keys` from the config (`cmd.Before` records the command line).

#### 11.3 Statement tab ✅

- [x] Statement tables open, Relations open under each, details in the alias form
      (`→ customer  o.customer_id`; nested: `customer.region_id`); the flat list is gone. The
      same succinct form in Objects.

#### 11.4 Search ✅

- [x] Omni-like in every tab: the tab's kind by default (all tables & views for an empty
      query in Objects/Statement), prefixes `:` `.` `#` `~` `@` `*`; columns from
      `sqlh/allColumns` (loaded on first use; `.orders.tot` matches table.column). Enter goes
      to the result in its tab (a column opens its table and selects it once loaded), Ctrl-Y
      inserts.

#### 11.5 Enter / Ctrl-Y ✅

- [x] Enter navigates (expand, connect → tab 2, open file, history versions); Ctrl-Y inserts;
      without an editor (`sqlh tui`) it copies with a popup; replace-statement in the `.` menu.

#### 11.6 Sample ✅

- [x] One row as `column  value` (wrapped), `]`/`[` through the fetched rows; the
      distinct-values grid and `v` are gone; `s` on a column kept.

#### 11.7 Docs/tests ✅

- [x] doc/cli.md (Panel rewritten with the key table, sqlh tui, no db/schema), README,
      config.example.yml; `~/.config/helix`: `space i D`/`S` removed, README updated. Tests:
      tui (tabs, search prefixes, form flows, sample row, history versions, configurable keys,
      copy popup), connedit, fields, reload, all columns, header error, editConfigFile.
- [x] Verified live in Herdr: tui starts clean on Objects; duplicate shop_prod → form →
      shop_test (dev) written into the config's PROD group; Enter connects → Objects; `.tot`
      search → orders.total; Sample one row; `n` opens Helix without the Nushell error, no log
      lines; Statement follows that Helix, Ctrl-Y on a relation inserts the join; the panel
      from Helix starts on Statement, Enter expands, Ctrl-Y inserts, closes with Helix.

#### 11.8 Later — relationship overview widget (idea, not scheduled)

An overview of the statement's relationships as a 4-column table, the left table named once:

```
sds_data_raw   ai_extraction_id  id                ai_extraction_process
               id                sds_data_raw_id   batch_document
               input_id          id                sds_extraction_input
```

Navigable on table names: choosing a foreign table makes it the left column with its own
relationships; Back returns to the previous table (a breadcrumb stack). Interactive, so it
needs its own place in the flow (a tab or a full-screen view) — revisit after 11.2.

#### 11.9 Decided (2026-10-16)

1. Headers with `db=`/`schema=` are an **error** (few files; the user fixes them). Objects shows
   the connection's own schema; the other `schemas:` entries as folders below it.
2. Groups: one level (`group: CADE`), ungrouped connections at the top level.
3. Connection form in the TUI writing config.yml, with **New**, **Duplicate**, **Edit** and
   **Delete**; `passwdCmd` editable as a plain field (passwords masked).
4. Queries tab: only `tui.queriesDir`.
5. Search is omni-like and works in every tab; with no prefix it searches **the tab's own kind**
   (Connections → connections, Objects/Statement → tables & views, Queries → files, History →
   runs). Prefixes: `:` tables & views, `.` columns (`table.col` too), `#` indexes/constraints,
   `~` sequences, `@` connections, `*` everything. Choosing a result of another kind switches
   to its tab.
6. Enter on a connection makes it current and goes to tab 2; Enter on a history run expands
   its versions, "replace statement" is in the `.` menu.
7. Ctrl-Y without an editor copies and says so in a popup.
8. The sample row is the first the database returns; `s` on a column (most common values)
   stays.
9. Keys under each part: `panel.keys.<action>` (panel and `sqlh tui`), `picker.keys.<action>`.

### Phase 12 — panel polish, round 2 (2026-10-16) ✅ done (branch `tui-experiment`)

Feedback after using Phase 11; decisions in 12.9. Built as planned, except: a group row keeps
the preview pane (an empty one; a layout that changes while moving through the tree jumps), and
the History rows' one-line SQL stays dim (not highlighted). Status per item in 12.10.

#### 12.1 Connections as an aligned table

Today `alias  db › schema  env` runs together, and the `●` of the current one shifts its row.

- Columns, widths taken over all rows (a group's indentation counts toward the first):
  `marker icon alias │ database │ schema │ env`, padded; no `›`. The env chip has a fixed
  width (`prod`, `test`, `dev ` padded), so all chips line up.
- The current connection: `●` in a marker column of its own before the icon, and a bold alias.
- Narrow panes: schema, then database, are cut with `…` first; the alias is never cut below
  12 cells; under ~40 columns only the alias and env.
- SQLite: database = the file name, schema empty.
- Possibly a dim header row (`alias  database  schema  env`) and a driver column (`pg`, `ora`,
  `lite`); see 12.9.

#### 12.2 SQL syntax highlighting

- New `internal/tui/sqlhl.go`: tokens from the existing `token` lexer (dialect of the
  connection), then light classification:
  - keyword; comment (the `-- sqlh:` header included); string; number;
  - function (an identifier followed by `(`); type (after `::`, in `CAST(… AS t)`, column types
    in DDL);
  - alias (the identifier after `AS`, or after a table name in `FROM`/`JOIN`); parameter
    (`:x`, `$1`, `?`); operators and punctuation dim.
- Used everywhere SQL is shown: DDL, a view's definition in Describe, a history run's SQL, the
  version diff (`+`/`-` gutter coloured, the code highlighted), the query-file preview (12.6),
  and the history rows' one-line SQL (dimmer when not selected).
- Wrapping after highlighting, ANSI-aware (`ansi.Wrap`); lines cut at the width stay styled.
- Colours by default from Helix's `modus_vivendi_tinted`, the theme Helix uses here:
  - keyword `#b6a0ff`, comment `#ff9f80`, string `#79a8ff`, number `#00bcff`;
  - function `#feacd0`, type `#6ae4b9`, alias `#00d3d0`, punctuation `#989898`.
- Override in `panel.syntax.<class>: "#hex"`, or read Helix's own theme (12.9).
- `icons: ascii` / `NO_COLOR`: no colour.
- Tests: the classes of a sample statement (golden: class per token), wrap keeps the styles.

#### 12.3 Collapse all

- New actions:
  - `collapseAll` collapses everything to the top level: Statement → the statement's tables;
    Objects → Tables / Views / … and the other schemas; Connections → the groups; History →
    the groups.
  - Possibly `expandAll`, to one level below the top (see 12.9).
  - The selection moves to the nearest row still visible.
- Statement: after `collapseAll`, tables that join the statement later arrive collapsed too, so
  following the cursor doesn't reopen everything. This lasts until `expandAll`, or until a
  table is opened by hand. Whether the Statement tab should *start* collapsed: see 12.9.
- Also on the tab's group rows, by mouse? No: keys and the palette (12.4) only.

#### 12.4 The actions palette (`?`), actions per tab

Modelled on easyredmine-tui's `HelpOverlay` (`internal/tui/help.go` there), ported to Bubble
Tea v1 / bubbles textinput.

- A box over the panel:
  - a title (the tab and the selection);
  - the list in groups, each entry `key  description`;
  - a filter input, with the focus in it at once;
  - a hint and an "n of m" count.
- Filtering: typing filters on the description (substring hits first, then fuzzy); `@…`
  filters on the keys.
- Keys: Enter runs the entry (as if its key were pressed); ↑/↓, Ctrl-J/K, PgUp/PgDn move; Esc
  clears the filter, then closes; `?` with an empty filter closes.
- Groups, in this order:
  1. **Selection: <label>** — what the `.` menu offers for the highlighted node (insert join,
     SELECT *, columns, DDL, sample; connect/edit/duplicate/delete; replace/insert/open…).
  2. **<Tab>** — the tab's own actions (Connections: new connection; Queries: new query;
     History: open result, open file; Statement/Objects: sample, refresh).
  3. **Navigation** — move, expand/collapse, collapse all, preview tabs and scrolling, the main
     tabs.
  4. **Global** — search, back to editor, help, quit.
- Every action gets an availability:
  - the main tabs it belongs to, and the node kinds it applies to;
  - outside them its key does nothing (a short "not in this tab" note) and it is left out of
    the footer and the palette.
- Today's single-key `.` menu: kept as the quick menu, or folded into the palette (12.9).
- `panel.keys` stays flat (action → keys).
  - Clashes are checked within the actions available together: global against every tab,
    then per tab.
  - So the same key may serve different actions in different tabs (e.g. `d` duplicate in
    Connections, DDL in Objects), if wanted (12.9).
- Tests:
  - the palette lists only the tab's actions;
  - the filter, `@` key filter, Enter runs, Esc semantics;
  - an unavailable key is a no-op;
  - per-tab clash validation.

#### 12.5 The slow handover when `sqlh tui`'s Helix exits

Cause: not Helix's setup as such.

- Helix starts its language servers as child processes and on `:q` sends them
  `shutdown`/`exit`, so the editor's sqlh server ends with it (normal LSP lifecycle; nothing
  in `languages.toml` keeps it).
- The delay is sqlh's own recovery:
  1. `watch()` sleeps 1 s before its first retry;
  2. `connect()` then waits another 1.5 s (in case an editor is starting);
  3. it starts a new headless server, which opens the database again;
  4. the TUI throws its caches away on `reconnected` and loads the catalog again (seconds on
     Oracle).

Fix:

- [ ] **Warm standby.**
  - On `sqlh/release` the headless server closes only its control socket and keeps its
    database connection, catalog and sample pool. While Helix is open that is one extra idle
    session.
  - When the editor's server goes, `startHeadless` takes the socket back at once, with the
    warm state:
    - config re-read if it changed;
    - the workspace's remembered connection followed (the editor may have switched);
    - a background catalog refresh if the editor ran DDL meanwhile (history has the runs).
- [ ] **No blind waits.** The backend knows why the server went:
  - if ours was released, an editor is starting: poll for its socket every 100 ms (3 s at most);
  - if the editor's server went, take over at once;
  - retries back off from 100 ms, not 1 s.
- [ ] **The UI stays usable through it.**
  - Trees, catalog and previews stay; on reconnect to the same connection the caches are
    refreshed in place, not reset.
  - Actions during the gap say "reconnecting…" instead of failing on a closed connection.
- Goal: under ~300 ms from `:q` to a working app.
- Test: a fake editor server that goes away → the backend serves again within the bound, and
  the warm catalog is not reloaded (load counter).
- (Rejected: making Helix's LSP a thin proxy to the app's server — a large change for this.)

#### 12.6 Queries tab preview

- A query file's preview:
  - the relative path, modified time, size;
  - the header's connection (marked when it isn't the current one);
  - the file highlighted (12.2), scrollable with J/K; no 10-line cap, the pane cuts it.
- Possibly the file's last run from history (when, rows, status), as in History (12.9).
- Read by the TUI directly (it's a local file; at most 256 KB, more is cut with a note).

#### 12.7 Preview tabs by selection

- The preview's tabs depend on what is highlighted:

  | selection | tabs |
  |---|---|
  | table, view, relation, statement table | Describe · DDL · Sample |
  | column | Describe (its table, the column marked) · Sample (most common values) |
  | index, constraint, sequence, type, trigger | Describe (· DDL when the server has it) |
  | connection | Connection |
  | query file | Query |
  | history run | Run |
  | group / info row | none (the tree gets the height) |

- Tab/Shift-Tab cycle only through the tabs shown.
- The chosen tab is remembered per kind of selection: DDL on tables stays DDL after visiting a
  query file.
- A single tab shows just its name in the separator line, with no `│` choices.

#### 12.8 Docs, tests, live check

- doc/cli.md: the Panel section (connections table, palette, collapse all, preview tabs, colours).
- README and the config example (`panel.syntax`, new actions).
- Tests as listed per item.
- Live check in Herdr:
  - `:q` of the app's Helix → the app is usable at once;
  - highlighting in DDL, History and Queries;
  - the palette per tab.

#### 12.10 Status ✅

- [x] 12.1 `connLayout`/`connRow`/`connHeader` (view.go): header row, rune-aligned columns
      (test checks the header over the row), `●` column, chip padded to 4.
- [x] 12.2 `sqlhl.go`: own lexer (comments, `'…'`, `$$…$$`/`$tag$`, `"…"`, numbers,
      parameters `$1 :x @x ?`, `::`) + word classes from neighbours (keyword list of the
      completion + extras − name-like words; function before `(`; types; aliases after `AS` or
      a FROM/JOIN table); wraps at spaces, each line styled on its own; diff lines on a
      green/red background. `panel.syntax` validated (class, `#rrggbb`).
- [x] 12.3 `collapseAll` (H, every tab), `expandAll` (L, Statement), `scrollTop` (z; may scroll
      past the last page until `g`/H/L), `panel.statement: open | collapsed`.
- [x] 12.4 `help.go`: the palette (groups: the tab's own, Navigation, Preview, Global; filter,
      `@` keys, Enter runs, Esc clears/closes, `?` closes). `panel.keys` sections per scope
      (`config.PanelKeys`, `PanelScopes`, `PanelScopeActions`); keymap per scope in the TUI
      (`PanelSettings.Keys` scope → action → keys); a key of another tab notes where it works.
- [x] 12.5 `Server.Resume` (warm standby: `sqlh/release` stops the socket and the running job
      only), `ranDDLSince` → background refresh; `panelBackend.watch` knows whether ours was
      released (wait up to 3 s for the editor's server) or the editor's went (take over at once),
      retries from 20 ms; the TUI keeps trees/caches on reconnect (`forgetErrors`; a catalog of
      another connection resets them). Live: `:q` of the app's Helix → serving again in under
      the 50 ms polling step, no `db worker: start` in the log. `TestHeadlessGivesWay` checks
      the same connection and cache after Resume, < 300 ms.
- [x] 12.6 `FileText` (256 KB cap), last run from the loaded history.
- [x] 12.7 `previewTabs`/`previewKind`, tab per kind remembered; History: **Query** / **Diff**
      (`sqlh/currentVersion`: the file's statement most like the run, from the live buffer,
      similarity ≥ 0.6).
- [x] 12.8 doc/cli.md (Panel, sqlh tui), README, config example; tests: TestPanelCollapseAll,
      TestPanelPalette, TestPanelKeysPerTab (incl. config errors), TestPanelPreviewTabs,
      TestSQLHighlight, connections alignment, history Diff tab. Live in Herdr: table, colours
      (truecolor `#b6a0ff` keywords in DDL), palette, handover.

#### 12.9 Decided (2026-10-16)

1. Connections table: a dim header row; no driver column.
2. Colours: built-in palette matching modus_vivendi_tinted, `panel.syntax.<class>` overrides.
3. `H` collapses all (every tab). `z` scrolls the selected row to the top of the view (vim `zt`).
   `L` expands all in the Statement tab only (not Objects). Statement opens as now by default;
   `panel.statement: open | collapsed` chooses.
4. The `.` menu stays as it is (selection actions); the `?` palette lists the keys/actions of
   the tab. Maybe merged later.
5. Keys per tab: `panel.keys` gets per-tab sections (`panel.keys.connections.<action>`, …,
   the flat `panel.keys.<action>` = all tabs); a key may mean different things in different tabs.
6. Query preview shows the file's last run when history has it (already loaded).
7. History preview: tab **Query** (the run and its SQL, default) and tab **Diff** (against the
   current version of that statement in the file / at the cursor).

### Phase 13 — panel polish, round 3 (2026-10-16) ✅ done (branch `tui-experiment`)

Feedback after Phase 12; decisions in 13.12, status in 13.14. Deviation: `ctrl+:` / `ctrl+R`
(decision 8) can't reach the panel — Bubble Tea v1 reads the classic terminal encoding, where
ctrl+R is ctrl+r and ctrl+: doesn't exist (no kitty keyboard protocol before Bubble Tea v2) —
so the defaults are `ctrl+e` / `ctrl+r`, and such keys are now config errors.

#### 13.1 Search: the kinds, visible

The prefix legend sits on the search's last line and is cut in a narrow pane
(`: tables . columns # indexes ~ sequences @ connect…`).

- [ ] The legend leaves the bottom line. While the query is empty (just `/`), the preview
      pane shows the legend instead of a preview: one line per kind, `prefix  kind  what it
      finds` (and the tab's default kind marked).
- [ ] Tab / Shift-Tab in the search cycle the kind (rewriting the prefix); the prompt line
      already names it (`indexes · 320`).
- [ ] The search's keys and the kinds are also in the `?` palette (search scope).

#### 13.2 Indexes and constraints apart

- [ ] `#` indexes only; a new prefix for constraints (13.12); triggers too, maybe.

#### 13.3 The `.` menu

- [ ] Its shortcut letters must not collide with navigation: "insert a join" `j` → `J`. The
      menu keeps j/k/↑/↓ for moving, so no menu letter may be j or k.
- [ ] The three inserts do what they say. Today "join" only applies after `JOIN`; elsewhere
      all three insert the name.
  - **insert** (= Ctrl-Y): after `JOIN` the join clause, else the name.
  - **insert the name**: always the name.
  - **insert a join**: after `JOIN`, `customer c ON c.id = o.customer_id`; elsewhere with the
    keyword, `JOIN customer c ON c.id = o.customer_id` (on its own line, the statement's
    indent). A table related to none of the statement's tables: an error, nothing inserted
    (13.12).
  - Labels say it: "insert (as Ctrl-Y)", "insert the name", "insert JOIN … ON …".

#### 13.4 Relations: a chain of joins, without loops

Today a related table opens to its own relations, and so on without end (batch_document →
exclusion_reason → batch_document → batch …).

The hierarchy below a table is a chain of joins (13.13): opening a relation is choosing the
next join; it stays an exploration tool, so a related table keeps everything a table shows
(relations, views, indexes, constraints, triggers, columns). Only the loops go:

- [ ] Below a relation, its Relations leave out the tables already on its path (the anchor and
      every hop): the tree ends where the chain can't grow. (Tables of the statement that are
      not on the path stay: how they relate is worth seeing.)
- [ ] Navigation stays h/l/Enter; no jump keys. (Ctrl-R / Ctrl-O are the inline picker's: a
      flat list needs keys to drill in and back; the tree has h/l.)
- [ ] Ctrl-Y anywhere in the chain inserts it up to that table, as now.
- [ ] Test: a cycle (a → b → a) is not offered; a 3-hop chain still is.

#### 13.4b Search in the Statement tab: the statement's own list

Today `/` in Statement searches all tables, and choosing one goes to the Objects tab.

- [ ] In Statement, `/` without a prefix is a fuzzy finder over the statement's tables (big
      queries), matched by name or alias (`orders`, `o`): it stays in Statement and selects
      the table's row. Other tabs unchanged (Objects still searches all its tables and views).
- [ ] `:` searches all tables and views, as today (goes to Objects).
- [ ] The legend (13.1) shows Statement's default kind ("this statement").
- [ ] Ctrl-Y on a result inserts as Ctrl-Y in the tree would.

#### 13.5 Collapse / expand one level at a time

- [ ] `H` closes the deepest open level of the tab: open relations, then the open tables,
      then the groups, down to the top level. Pressed again, the next level.
- [ ] `L` (Statement) opens the next level: tables, then their groups.
- [ ] Statement: "tables arrive collapsed" holds once `H` reaches the tables level, until `L`
      opens it again.

#### 13.6 Queries: rename, delete

- [ ] `R` rename: a one-line form prefilled with the name (`.sql` added if missing; another
      folder allowed, relative to the queries folder). `X` delete: asks first.
- [ ] A file open in the attached Helix: rename through Helix (`:move <new>`, which renames
      its buffer and the file); delete refused ("open in the editor") — see 13.12.
- [ ] History records the file path. After a rename, the file's earlier runs no longer match
      it; see 13.12.
- [ ] Also in the `.` menu of a query file.

#### 13.7 Connections as a standalone command

- [ ] `sqlh connect` without an alias (Helix `space i C`) opens the Connections tab on its own,
      instead of the inline picker:
  - the table, groups, preview and filter (`/`);
  - Enter connects the editor (and points the file's header at it, as now) and exits;
  - Esc / `q` exits without a change;
  - C/E/D/X work as in the panel.
- [ ] Where it draws: see 13.12.
- [ ] `sqlh connections -p` likewise (prints the alias instead of switching).

#### 13.8 History: opening a run's file that is gone

- [ ] `e` (and Enter on a file-less, non-expandable run…) on a run whose file no longer
      exists: recreate it at its path (folders too), with the connection header of the run
      (`-- sqlh: conn=<alias>`) and the run's SQL, then open it. A note says it was recreated.

#### 13.9 History: the current connection's runs

- [ ] The recent runs list only those on the current connection (`e.Conn`). A key toggles
      "all connections" (13.12); the group/title says which (`Recent · pg_local`).
- [ ] This statement's versions: see 13.12.

#### 13.10 The config, from the app

- [ ] `editConfig` (global): opens the config file the server reads (`Status.Panel.ConfigPath`)
      in `$VISUAL` / `$EDITOR` (else `vi`) in the panel's own pane (Bubble Tea suspended,
      `tea.ExecProcess`). Back in the app, it reloads (below).
- [ ] `reloadConfig` (global): the server re-reads the file (`sqlh/reloadConfig`); the app
      re-reads its settings (`sqlh/status` → keys, icons, syntax, statement, editor, queries
      folder) and rebuilds its keymap and trees. A broken file: the error in a popup, the old
      config stays (fix it with `editConfig` again).
- [ ] Keys: see 13.12.

#### 13.11 Docs, tests, live check

- [ ] doc/cli.md, config example; tests per item (the menu's join outside `JOIN`
      [`panelText`], relation → reveal + `ctrl+o`, H/L steps, rename/delete with a fake
      editor, recreate, history filter, reload applying new keys); live check in Herdr.

#### 13.12 Decided (2026-10-16)

1. Search: the legend in the preview while the query is empty; Tab / Shift-Tab cycle kinds.
2. Prefixes: `#` indexes, `!` constraints, `^` triggers.
3. "insert a join" for a table related to none of the statement's: an error.
4. Relations (13.4): the loop-free chain — below a relation, no table already on its path;
   everything else a table shows stays; h/l navigate, no jump keys.
9. Statement search (13.4b): `/` finds among the statement's tables (name or alias) and stays
   in Statement; `:` for all tables. Only Statement changes.
5. Rename via Helix `:move` when the file is open in the attached Helix, else on disk (also
   without any editor); delete refused while open. History may lose the link after a
   rename: a later "recreate the old file" function retrieves the old SQL.
6. `sqlh connect` (no alias): a small list — the 4-column table with filtering — as a popup in
   the middle of the screen or at the cursor (like today's picker).
7. History: `c` toggles all connections for the recent runs. "This statement" is not
   filtered (it belongs to the file, whose header names the connection).
8. `editConfig` default `ctrl+:`, `reloadConfig` default `ctrl+R` (both global, both in the
   `?` palette).

#### 13.14 Status ✅

- [x] 13.1 legend in the preview while the query is empty (`searchLegend`), Tab/Shift-Tab
      cycle kinds (`searchNextKind`/`searchPrevKind`, search scope); the cut-off bottom line
      gone. The search's keys are not in the `?` palette (`?` types into the query); the legend
      names them.
- [x] 13.2 `#` indexes, `!` constraints, `^` triggers (`objectKind`); reveal by table id (the
      table's node is rebuilt when it loads).
- [x] 13.3 menu: `J` join, `Q` new query file (`q` closed the menu); `panelText` → error and
      `joinClause`; join mode outside `JOIN` writes `\n<indent>JOIN …` (or `JOIN …` at a line
      start); the panel checks first (`joinable`, from the statement's relations).
- [x] 13.4 `onPath`: a relation's relations (and views) leave out its chain's tables.
- [x] 13.4b `skStmt`: the statement's tables, revealed in Statement.
- [x] 13.5 H: deepest open visible level; L: shallowest closed (Statement); `stmtCollapsed` when
      H reaches the tables / L opens them.
- [x] 13.6 `R`/`X` in Queries (+ menu); `RenameQuery` (Helix `:open` + `:move` when the file is
      open — `sqlh/fileOpen` asks the server — else `os.Rename`), `DeleteQuery` refuses an
      open file; prompt box (`prompt.go`).
- [x] 13.7 `tui.ConnectionPickItems` (aligned table, env chips in raw ANSI: the picker draws on
      /dev/tty), `sqlh connect`, `sqlh connections -p`; standalone connections carry the
      target/env/group. Fuzzy: a consecutive run now beats jumping to a word start (`shop`
      ranked `shop_prod` over `shop`).
- [x] 13.8 `EnsureFile` + `editRunFile`.
- [x] 13.9 `historyConn`, `c` (`historyAllConns`), subtitle says which; search over runs follows.
- [x] 13.10 `editConfig` (`tea.ExecProcess` $VISUAL/$EDITOR/vi) → `reloadConfig`
      (`ReloadSettings`: `sqlh/reloadConfig` + status → keymap, icons, colours, statement);
      an error stays in a popup until a key. `receivable` validates keys.
- [x] 13.11 docs; tests: TestPanelSearchKinds, TestPanelStatementSearch,
      TestPanelRelationsNoLoop, TestPanelMenuJoin, TestPanelQueryRenameDelete,
      TestRenameDeleteQuery, TestPanelRunFileRecreated, TestPanelHistoryByConnection,
      TestPanelReloadConfig, TestConnectionPickItems, TestPanelText (join cases); live in
      Herdr: `space i C` table + filter + switch, Statement `/`, legend, H/L steps.

#### 13.13 Note: how nested relations build join chains (for decision 4)

A relation row carries its path from the table it was opened under (`anchor` + `path`
hops). Ctrl-Y on a nested one, with the cursor after `JOIN` and the anchor in the
statement, inserts the whole chain (`panelText` → `catalog.ComposeJoin`):

```
orders o                     (in the statement)
  Relations
    → customer               (hop 1: o.customer_id)
      Relations
        → region             (hop 2: c.region_id)   Ctrl-Y here →
customer c ON c.id = o.customer_id
JOIN region r ON r.id = c.region_id
```

The loop (batch_document → exclusion_reason → batch_document …) comes from offering every
relation at every level, the chain's own tables included. A cycle-free variant keeps chains:
below a relation, leave out the tables already on its path (and the statement's), and show
only its Relations (no indexes/constraints/columns clutter); a separate key goes to the
table in Objects.

### Phase 14 — the connections popup, join-chain search (2026-10-16) ✅ done (branch `tui-experiment`)

Decisions in 14.4, status in 14.5.

#### 14.1 `space i C`: the popup's highlighted row misaligned

The inline picker draws the highlighted row as `▌` + one mark cell + the text stripped of
ANSI; the other rows as a space + two mark cells + the text. So the highlighted row sits one
cell left of the others (and of the header), and its env chip loses its colour.

- [ ] Picker: the highlighted row gets the same prefix width as the others (`▌` + two cells).
      All pickers benefit (tables, columns, …).
- [ ] Header offset: the header line lines up with the rows' text (today 1 cell short).
- [ ] The env chip keeps its colour on the highlighted row: `PickItem.Tail` (a styled suffix
      the selection background doesn't strip), used for the chip.
- [ ] Closer to the app's table (optional, 14.4): the `●` of the current connection in a
      column after the alias (as in the Connections tab), the connection icon before it.
- [ ] Test: every row's columns (highlighted or not) and the header's start at the same cells.

#### 14.2 Statement search: `:` finds join chains

In the Statement tab, `:` today searches all tables and views — the same as Objects' `/`.
Instead it finds **tables reachable from the statement's tables by a chain of
relationships**, to find a join path quickly:

- [ ] Server: `sqlh/chains` (control method) on the relationship graph the server already
      holds (`catalog.Relations` per table, in memory: no database queries):
  - `{From: the statement's tables, MaxDepth}` → every reachable table not in the statement,
    with its distance (fewest joins) and how many shortest chains reach it (breadth-first,
    the loop-free rule of 13.4: a chain never revisits a table);
  - `{From, Target}` → the chains to that table (anchor + hops with columns), shortest first,
    capped (14.4).
  - Only joinable relationships (with columns: foreign keys, view-inherited, `maybe` when
    `relations.maybe` is on); not "used by views".
- [ ] Search list (`:` in Statement): the reachable tables, nearest first, each with the
      shortest chain as detail (`orders → customer → region · 2 joins`, `+2 more`).
- [ ] Enter on a result: the Statement tree opens along each of its chains — the anchor table,
      its Relations, the first hop, its Relations, … the target, selected (the first chain's).
      Relations load lazily on the way (a pending path walk, like the pending reveal, continued
      after each load). Other open branches: see 14.4.
- [ ] Ctrl-Y on a result: inserts the shortest chain (the same as Ctrl-Y on its row after the
      walk); several equally short: 14.4.
- [ ] Objects keeps `:` as today (and `/` in Statement still finds the statement's tables).
- [ ] The legend and Tab cycling show the kind in Statement as "join chains".
- [ ] Tests: graph fixtures (a → b → c, a → d → c: two shortest chains to c; a cycle b ↔ e;
      the depth cap), the walk opening both chains in the tree, Ctrl-Y inserting the chain.

#### 14.5 Status ✅

- [x] 14.1 picker: the highlighted row `▌` + the two mark cells (as the others); its colour
      profile from /dev/tty (`termenv.NewOutput(tty).EnvColorProfile()` → the default
      renderer: the selection's background and the dims show under `:pipe-to`);
      `PickItem.Tail` keeps the env chip on the highlighted row. The connections table: the
      icon, `●` after the alias, header at the rows' cells (test draws the picker and checks
      every row's columns, highlighted included).
- [x] 14.2 `catalog.Chains` (breadth-first over `catalog.Relations`, joinable edges only,
      statement tables at distance 0 so never passed through; counts of shortest chains;
      chains to a target in the from/relationship order, capped), `sqlh/chains`,
      `relations.chainDepth` (4). TUI: `skChain` (`:` in Statement via `tabSpecific`), list
      loaded once per set of statement tables, `walkPath` opens anchor → Relations → hop → …
      across lazy loads (continued from `rebuild`), Ctrl-Y inserts `Mode: join` with the
      chain. Tests: TestChains (two shortest chains, a two-way relationship, depth cap, two
      statement tables), TestPanelChainSearch. Live: `:` in Statement listed customer,
      order_line, v_orders, region (2 joins); Enter opened orders → customer → region;
      Ctrl-Y after `join` inserted `customer c ON …` + `JOIN region r ON r.id = c.region_id`.

#### 14.3 Order of work (14.4 Q4)

Recommended: 14.1 and 14.2 first, then the Bubble Tea v2 port (Phase 16) on the finished
code, with a commit in between.

- Neither depends on v2: 14.1 is a few lines of the picker; 14.2 is server logic (graph
  search), tree walking and the search list — plain Go, not library API.
- The v2 port is mechanical but touches every view, key and mouse handler: done once, over
  the final code, with the whole test suite (33 TUI tests, the cmd and handler ones) as the safety net, and a
  known-good commit to compare against if something regresses.
- Only the `ctrl+:` / `ctrl+R` defaults wait for v2.

#### 14.4 Decided (2026-10-16)

1. Popup: the alignment is required; the highlighted row as a coloured row (like the app)
   rather than only the `▌` is wanted if it comes cheaply; the rest (icon, `●` column) nice to
   have. No 5th column (the group stays a dim `CADE/` prefix).
   Found while planning: the highlighted row *is* drawn with the selection background, but
   under Helix's `:pipe-to` stdout is a pipe, so lipgloss decides there are no colours —
   the picker must take its colour profile from /dev/tty.
2. Chains: at most 4 joins by default, `relations.chainDepth`.
3. Enter opens only the shortest chain (the first, 4.) to the table and selects it; the rest
   of the tree stays as it is.
4. Ctrl-Y with several equally short chains: the first (the focused table's first, then the
   statement's order).
5. Order: Phase 14, a commit, then the v2 port (now Phase 16; Phase 15 added before it).

### Phase 15 — join chains: all of them, n/N between them (2026-10-16) ✅ done (branch `tui-experiment`)

Feedback on 14.2: one (shortest) chain isn't enough. Different chains to the same table
don't mean the same rows: `sds_pdf_file → sds_data_raw_english` directly vs through
`batch_document` — not every extraction goes through a batch, so the batch chain gives
fewer rows. So the user wants to see every chain, and to step through them. Open questions
in 15.5.

#### 15.1 All chains to a table (server)

- [ ] `sqlh/chains` with a target returns every simple chain (no table twice, never through
      another statement table) from the statement's tables to the target, up to
      `relations.chainDepth` joins, shortest first (then the statement's order, then the
      relationships' order), capped (15.5).
- [ ] Enumeration pruned by distance: a breadth-first pass from the target gives each
      table's distance to it, and the depth-first walk drops a branch as soon as
      `joins so far + distance to target > chainDepth` — no blow-up on large schemas
      (CADE: hundreds of tables, ~10 relationships each).
- [ ] The reachable list (no target) stays the cheap breadth-first pass: nearest distance and
      the shortest chain; the number of chains is counted when one is chosen (15.5 Q4).

#### 15.2 Enter opens them all; n / N step through them

- [ ] Enter on a `:` result opens the Statement tree along **every** chain to the table (the
      walk of 14.2 per chain; shared prefixes open once) and selects the target at the end of
      the first (shortest) one.
- [ ] A **chain set** stays active: the footer shows `chain 1/5 · n/N next/prev · esc
      clear`; `n` selects the target at the end of the next chain (scrolling it into view),
      `N` the previous, wrapping. The chain's rows are marked (accent bar or colour) so the
      path to the selected target is visible among the open branches.
- [ ] Cleared by Esc in the tree, a new search, another tab's `:` result, or the statement's
      tables changing. The opened branches stay open (`H` closes them).
- [ ] Ctrl-Y on a target row inserts *that* chain (as today: the row knows its path); Ctrl-Y
      on a `:` search result still inserts the shortest one.
- [ ] Actions `nextChain` / `prevChain` (Statement scope), keys `n` / `N`; in the `?` palette.

#### 15.3 New query moves to `c`

- [ ] `newQuery` default `n` → `c` ("create query") in Objects, Statement and Queries, so `n`
      is free for the chains (and reads like vim's search navigation). History's `c` (all
      connections) is another tab: no clash. The `.` menu keeps its own letters (`Q` new
      query file; `n` there is "insert the name").
- [ ] Docs, hints (Queries subtitle "c: new query", the empty Queries tab's "c creates
      query_1.sql", Statement's "no editor: … create one (c)").

#### 15.4 Tests

- [ ] catalog: all chains to a target (the direct and the two-step chain both listed, shortest
      first; the cap; pruning keeps chains exactly at the depth limit; never through a
      statement table).
- [ ] TUI: Enter opens every chain, `n`/`N` cycle the targets (wrapping), Esc clears,
      Ctrl-Y on the 2nd chain's target inserts the 2nd chain, `c` creates a query.

#### 15.6 Status ✅

- [x] `catalog.Chains` rewritten: breadth-first for distances and order, depth-first for every
      simple chain (counts per table up to the limit, `More` past it; a 200k-step budget);
      with a target, pruned by a breadth-first distance-to-target over the edges seen (both
      ways: a lower bound), shortest first, capped (`ChainsResult.More`).
      `relations.chainLimit` (10), `panel.chains: one | all` (`PanelSettings.ChainsAll`).
- [x] TUI `chainSet` (chains, current, each chain's row ids once walked, a queue for `all`):
      `walkPath` per chain across lazy loads; one at a time closes the previous chain from
      the first relation it doesn't share with the next; the current chain's rows marked;
      footer `n/N chain 2/5 · esc clear`; cleared by Esc or when the statement's tables
      change. `nextChain`/`prevChain`/`clearChains` (n, N, esc; Statement). `newQuery` → `c`
      (hints follow the key).
- [x] List detail `3 chains · 2 joins` / `10+ chains`.
- [x] Tests: TestChains (all chains, longer ones after the shortest, limit/More, depth,
      two statement tables), TestPanelChainSearch, TestPanelChainSteps (one and all: n,
      wrap, N, the closed branch, Ctrl-Y on the 2nd chain, Esc, `c`). Live (shop): from
      `order_line`, `:` listed `customer 2 chains · 2 joins`; Enter opened order_line →
      orders → customer, `n` closed it and opened orders → v_orders → customer (chain 2/2).

#### 15.5 Decided (2026-10-16)

1. Every chain up to `relations.chainDepth` joins (default 4).
2. At most `relations.chainLimit` chains per table (default 10).
3. `panel.chains: one | all` — open one chain at a time (default: `n` closes the previous
   chain's branch where it leaves the next one's path and opens the next), or all at once.
4. The `:` list shows the number of chains and the shortest length (`3 chains · 2 joins`,
   `10+ chains` past the limit), not the steps (seen in the tree once chosen).
5. Before the v2 port (Phase 16).

### Phase 16 — Bubble Tea v2 (2026-10-16) ✅ done (branch `tui-experiment`)

Why: v2 reads the kitty keyboard protocol, so keys like `ctrl+:` and `ctrl+R` (= ctrl+shift+r)
reach the panel; v1 reads only the classic encoding. Probed in Herdr (`/tmp/keyprobe`, v2.0.10):
Herdr grants the app's keyboard-enhancement request (`Flags:1`, disambiguate) and forwards
`ctrl+r`, `ctrl+shift+r`, `ctrl+:` (code `:`), `ctrl+shift+;` and `ctrl+;` as distinct keys
(sent with `herdr pane send-keys`; a real Ghostty keypress confirmed in 16.2).

#### 16.1 The port

- [ ] Modules: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`
      (textinput, cursor), `charmbracelet/x/ansi` updated. easyredmine-tui (v2.0.2, its
      help palette, `keyPressFor`) as the reference for the patterns.
- [ ] Models: `View() tea.View` (alt screen, mouse mode and keyboard enhancements set on the
      view, not as program options); `tea.KeyPressMsg` (`String()`, `Keystroke()`, `Code`,
      `ShiftedCode`, `Mod`); mouse: `MouseClickMsg` / `MouseWheelMsg`; `tea.ExecProcess` as is.
- [ ] Colours: lipgloss v2 has no `AdaptiveColor`; light/dark from `tea.BackgroundColorMsg`
      (`lipgloss.LightDark`), the styles built once the background is known; the colour
      profile is tea's (downsampled on output), which also fixes the picker's colours on
      /dev/tty (no raw-ANSI chips needed).
- [ ] The inline picker on /dev/tty: v2's inline (non-alt-screen) renderer; it negotiates the
      keyboard protocol itself — the manual push/pop of kitty flags around it (for Helix)
      reviewed: Helix must get its own flags back after the picker.
- [ ] Keys: config names stay as they are (`ctrl+y`, `G`, `space`, `shift+tab`, `]`); a
      matcher maps a key press to every name it can have (`ctrl+R` = `ctrl+shift+r`,
      `ctrl+:` = ctrl + `:` or ctrl+shift+`;`, `G` = `shift+g`), so configs keep working.
      `receivable` (13.10) relaxed accordingly.
- [ ] Defaults: `editConfig` `ctrl+:`, `reloadConfig` `ctrl+R` (16.2: no fallbacks).
- [ ] Tests: the key helper builds `tea.KeyPressMsg`; everything else unchanged; `-race`.
- [ ] Live: panel, `sqlh tui`, pickers from Helix (`space i T`, `space i C`), the form,
      mouse clicks on the tab bar, `ctrl+e`/`ctrl+:` editing the config.

#### 16.3 Status ✅

- [x] `charm.land/bubbletea/v2` v2.1.0, `lipgloss/v2` v2.0.6, `bubbles/v2` v2.2.1; v1 and
      termenv gone (`go mod tidy` raised the `go` directive to 1.26.0, required by them).
- [x] `View() tea.View` (panel: `AltScreen`, `MouseModeCellMotion`; picker: inline); tests
      use `render()`. `tea.KeyPressMsg` everywhere; `keyNames` (String, Keystroke, shift
      folded in: `ctrl+R`, `ctrl+:` via the US shifted map, `ShiftedCode` when the terminal
      reports it) + `keymap.actionOf`; `typed` for text input; `tea.PasteMsg` to the search,
      palette, prompt, form; mouse `MouseClickMsg`/`MouseWheelMsg`; the v1 "jj in one message"
      split dropped (v2 parses keys one by one).
- [x] Styles: `setStyles(dark)` with `lipgloss.LightDark`; the panel and the picker ask
      `tea.RequestBackgroundColor` and rebuild on `BackgroundColorMsg`. Colour profile from
      the program's output (the picker's /dev/tty): the v1 `SetColorProfile` workaround and
      the manual kitty push/pop around the picker removed (v2 pushes its flags and pops them
      on exit: Helix's come back).
- [x] bubbles textinput: `staticCursor` (styles' `Cursor.Blink = false`), `SetWidth`. Boxes
      with a set width: `Width(bw+2)` (v2 counts the border).
- [x] Keys: `mark` default `space` (v2's name; `" "` in a config still works), `editConfig`
      `ctrl+:`, `reloadConfig` `ctrl+R`; `receivable` removed.
- [x] Tests: `press(name)` builds kitty-style key presses (`ctrl+R` = ctrl+shift+r, `ctrl+:` =
      ctrl+shift+;, `G` = shift+g with text); all pass with `-race`. Live in Herdr: `space i
      C` picker (Esc back to Helix intact), panel `:` chains + `n`, `ctrl+R` reloads,
      palette, `sqlh tui` tree and connection form.

#### 16.2 Decided (2026-10-16)

1. No backup keys: `ctrl+:` / `ctrl+R` only; the `?` palette is the way in elsewhere.
2. A real keypress in Ghostty → Herdr → v2: Ctrl-: arrives as `ctrl+shift+;` (code `;`,
   no shifted code: the layout's `:` isn't reported), Ctrl-Shift-<letter> as
   `ctrl+shift+<lower>`. So the matcher maps `ctrl+:` → `ctrl+shift+;` (US layout) and
   `ctrl+R` → `ctrl+shift+r`.

### Phase 5 — later / optional

- [ ] `EXPLAIN` to buffer
- [ ] Confirmation before destructive runs on `prod` connections (`DELETE`/`UPDATE` without
      `WHERE`, DDL), through the inline TUI (after Phase 10)
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

- **2026-10-16** Phase 7 done: fix for UNION lineage in derived tables/CTEs (+ Oracle
  `MINUS`); current schema per connection (`sqlh schema`, header `schema=`, CADE works from
  read-only logins); lineage for UNION views; go to definition of tables and `sqlh ddl`.

- **2026-10-16** Phase 16: Bubble Tea v2 (kitty keyboard protocol): `ctrl+:` edits and
  `ctrl+R` reloads the config; shift spellings matched either way.

- **2026-10-16** Phase 15: every join chain to a table (up to `chainDepth`, at most
  `chainLimit`), opened one at a time (or all), `n`/`N` between them; new query on `c`.

- **2026-10-16** Phase 14: pickers' highlighted row aligned and coloured (colour profile from
  /dev/tty), the connections popup like the tab; `:` in Statement finds join chains
  (`sqlh/chains`) and opens the tree along the shortest one.

- **2026-10-16** Phase 13: search legend + kinds (indexes/constraints/triggers apart), menu
  inserts fixed (`J`, join outside JOIN), loop-free relation chains, Statement-tab search,
  H/L by level, rename/delete query files, `sqlh connect` as the connections table, recreate a
  run's missing file, History by connection, edit/reload the config from the panel.

- **2026-10-16** Phase 12: connections table, SQL highlighting, collapse/expand all + `z`,
  `?` keys palette per tab with per-tab key config, preview tabs by selection (query file
  text + last run; run Query/Diff against the file now), warm headless standby (instant
  handover when `sqlh tui`'s Helix exits).

- **2026-10-16** Phase 11 (first-use feedback): tabs 1–5, omni search with prefixes, Enter
  navigates / Ctrl-Y inserts, connection form editing config.yml (connedit), groups,
  one-row samples, configurable keys (panel + picker); database/schema switching removed
  (a connection is the target; `db=`/`schema=` headers are errors); tui bugs fixed (Nushell
  hook env, log over the screen).

- **2026-10-16** Phase 10 built on `tui-experiment`: `internal/tui` (panel, inline picker),
  `internal/herdr` (socket API), `internal/history`; panel with object tree, statement
  context, Describe/DDL/Sample previews, history versions (replace the statement), `sqlh tui`
  with a headless server handing over to Helix's; fzf removed. Verified live in Herdr + Helix.

- **2026-10-16** Phase 10 planned after a lazydb comparison: own Bubble Tea TUI replacing
  fzf — side panel tied to the Helix pane (object tree, inserts at Helix's cursor through a
  pending action + `:insert-output sqlh panel take`), inline popup, `sqlh tui`; sample rows
  on a separate read-only session with a dev/test/prod policy; run history with versions.

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
- Edge dumps before/after a change: `CFG=<config> DUMP=out.txt go run ./scratch/zz_reldump ALIAS`
  (`PROBE_SCHEMA=S` pretends S is current), diff two dumps
- Definitions: `go run ./scratch/ddlprobe ALIAS SCHEMA NAME [skip]` (skip: no DBMS_METADATA);
  `cade2` (casys) may use DBMS_METADATA, `cadetst_read_only`/`cadedev_exp` may not
- Pooler mode of a PostgreSQL connection (read-only, no SET): `go run ./scratch/pgbprobe ALIAS`
- Schema switching / table definitions against PostgreSQL: `SQLH_TEST_PG=<dsn> go test
  ./internal/handler -run 'SwitchSchema|TableDefinition'` (needs two user schemas)
- Oracle end to end over LSP with only the read-only connection (the config's first one is
  `cadeprod`): `/tmp/sqlh-e2e-gd/e2e.py` (own config, runtime and state dirs)
