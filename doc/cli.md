# sqlh CLI

`sqlh` with no command (or `sqlh lsp`) is the language server. The other commands work
with the server of the current workspace — the same connection, session and **unsaved
editor buffers** — via a Unix socket. Without a running server, `run`, `export`,
`connections` and `latest` work standalone from the config. `dump` from the terminal always
runs standalone, on the connection in the file's header (see [Query files](#query-files-connection-header-and-dump)).

| Command | What it does |
|---|---|
| `sqlh run` | run SQL → CSV, first N rows (`--limit N`, default `results.previewLimit`; `--limit all`; `--limit ask` picks it); prints the CSV path(s) on stdout, summary on stderr; `--open`: open the CSV in the viewer; `--bg`: return at once |
| `sqlh export` | same, all rows |
| `sqlh dump FILE…` | run query files on the connection in their header → `<file>.csv` in the current directory (`-d DIR`, `-o FILE`), all rows; see [Query files](#query-files-connection-header-and-dump) |
| `sqlh connect [alias]` | switch the editor's connection (no alias: pick from the connections table — connection, database, schema, env — filtered as you type); remembered per workspace; `--file F`: also point F's connection header at it |
| `sqlh connections` / `ls` | list connections, `*` = current; `-p` picks one from the table and prints its alias |
| `sqlh status [--all]` | workspace, connection, current schema, running query, results dir (`--all`: all servers) |
| `sqlh cancel` | cancel the running query |
| `sqlh refresh` | reload schema metadata (done automatically after a run that executed DDL, unless `refreshAfterDDL: false`; for changes made elsewhere) |
| `sqlh latest` | path of the newest result CSV: `vd $(sqlh latest)` |
| `sqlh open [CSV…]` | open the newest result CSV (or the given ones) in `results.viewer`, see [Viewer](#viewer) |
| `sqlh tables [-s SCHEMA] [--all]` | tables/views as you'd type them (default schema unqualified, others `schema.table`, quoted when needed); `-p` picks one (preview: `describe`); with `--file F --line L --col C` the tables related to the statement's come first and after `JOIN` it inserts the join clause, see [Table picker](#table-picker) |
| `sqlh describe TABLE` | columns (foreign keys marked), relationships, a view's definition (`--json`) |
| `sqlh ddl TABLE` | a table's or view's CREATE statement(s), as go to definition shows it (`schema.table`, synonyms; `--json`; standalone with `--conn`) |
| `sqlh relations TABLE` | the tables and views related to TABLE, grouped (`--long`, `--json`; `-p` picks one) |
| `sqlh columns [TABLE] [--prefix p.] [--sep ", "]` | a table's columns; `-p` multi-picks (Tab, Ctrl-A all) → `p.a, p.b`; no TABLE with `-p`: pick the table first |
| `sqlh schemas [--all]` / `sqlh databases` | lists (`-p` picks one): the connection's schemas, the server's databases |
| `sqlh panel` | open (or focus) the side panel of this Helix pane: connections, objects, the statement at the cursor, queries, history; Ctrl-Y inserts at Helix's cursor (Herdr); see [Panel](#panel) |
| `sqlh panel take` | print what the panel chose, for the cursor (the hidden Helix binding `space i P`) |
| `sqlh tui` | the panel as an app, for the workspace around the current directory (`-C DIR`): `c` new query opened in Helix next to it; see [sqlh tui](#sqlh-tui) |

A connection is one database and schema: to work in another, add a connection for it (the
panel duplicates and edits connections). There is no switching of databases or schemas
within a connection, and a header with `db=`/`schema=` is an error.

## Current schema

A connection's current schema is where unqualified names resolve — in your queries and in
completion, hover, diagnostics, join hints, relationships and go to definition, which all
work in that schema (other schemas of the allow-list: tables and columns, `schema.table`).

- It starts on the connection's `schema:`, else the **first of its `schemas:`**, else the
  login's own (Oracle: the login schema; PostgreSQL: the DSN's `search_path`).
- Oracle: `ALTER SESSION SET CURRENT_SCHEMA` on every session sqlh opens. A read-only login
  (`cade_read_only`) works in `CADE` as if logged in as its owner — names resolve to what the
  login may see.
- PostgreSQL: `SET search_path TO <schema>, <the rest of the original path>` after
  connecting (extensions in `public` keep working). Set after connecting because PgBouncer
  refuses `search_path` as a startup parameter before PostgreSQL 18; fine with PgBouncer in
  session mode.
- The other schemas of `schemas:` are metadata: completion and the panel list their tables
  (`schema.table`). To work *in* another schema, make a connection for it.

## Pickers

`-p` (and `connect` without a name, `run --limit ask`) opens sqlh's own picker:
it draws on `/dev/tty` below the cursor (no alternate screen; the kitty keyboard protocol is
switched off around it) and prints the choice **without a trailing newline** when stdout is
not a terminal, so it can be inserted directly from Helix:

```
:insert-output sqlh tables -p
:insert-output sqlh columns -p --prefix p.
```

Typing filters (fuzzy, ranked by match quality; the input order holds while the query is
empty); ↑/↓ or Ctrl-P/Ctrl-N move, Enter accepts, Esc/Ctrl-C prints nothing (exit 0); in
multi-pick lists Tab marks and Ctrl-A marks all; Shift-↑/↓ scroll the preview. The keys are
`picker.keys.<action>` in the config (`accept`, `cancel`, `up`, `down`, `pageUp`, `pageDown`,
`previewUp`, `previewDown`, `toggleDown`, `toggleUp`, `toggleAll`, `clearQuery`, `deleteWord`;
the table picker's `nameOnly` Ctrl-T, `relations` Ctrl-R, `back` Ctrl-O). Without `-p`
the commands print one insertable name per line (`--long` for details, `--json` structured),
e.g. to feed another tool. `SQLH_PICK=<query>` picks without a UI (the best match; all
matches in multi-pick lists) — for scripts and tests.

Flags may come before or after positional arguments (`sqlh columns caproduct -p`).

## Relationships

`describe`/`relations`, the table picker, join completion and hover use the default schema's
relationship graph (schemas are boundaries: nothing across schemas):

- **foreign keys** (`→` this table references it, `←` it references this table);
- **views**: a view column that is a plain reference to a table column inherits that column's
  foreign keys, so `v_orders.customer_id` joins `customer` like `orders.customer_id` does, and
  a view exposing a table's primary key joins back to it. Views on views are followed down to
  the tables; views with `UNION` get none (their rows may come from elsewhere). These are
  listed for the view only — a table lists the views reading it (`◇`), which also join when
  they expose its key;
- **maybe**: a column named after another table's single-column primary key
  (`orders.customer_id` → `customer.id`, or `customer.customer_id` when the key is itself named
  after its table), only without a foreign key on it, when exactly one table matches and the
  types agree. `relations.maybe: false` turns them off.

Tables and foreign keys load with the connection; views (definitions parsed for lineage) with
the background load a few seconds later (`(views are still loading)` until then).

## Table picker

`sqlh tables -p --file F --line L --col C` (Helix `space i t` passes `%{buffer_name}`,
`%{cursor_line}`, `%{cursor_column}`; with a server the editor's unsaved buffer is used):

- the list shows table names only; the preview shows how the highlighted table joins
  (`o → customer.id = o.customer_id`), then the table like the editor's hover
  (`sqlh describe`): relationships as `→ customer.id = customer_id`, the related table first
  (highlighted), notes after (`nullable`, `via orders`, `maybe`, `1:1`). A table related twice shows the columns: `employee (o.created_by)`;
- the relationships of the statement's tables come first, then the other tables, dimmed
  (`--related`: only the related ones). With the cursor **on a table** of the statement the
  list starts with that table's relationships (Ctrl-O: the full list), and accepting
  **copies** the name to the clipboard instead of inserting it (inserting at the cursor
  would split the name under it); the editor's status line says so;
- system schemas (`pg_catalog`, `information_schema`) are left out, unless they are all there is
  (PostgreSQL's `postgres` database);
- typing ranks by match quality (only the names are matched);
- after `JOIN`, Enter inserts `customer c ON c.id = o.customer_id` (alias from the table's
  initials, numbered when taken; keyword case from `lowercaseKeywords`); elsewhere, or with
  `relations.insert: name` / `--insert name`, the name. **Ctrl-T** inserts the other one;
- **Ctrl-R** lists the highlighted table's relationships (prompt `o › customer`), and
  accepting there inserts the whole chain (the [panel](#panel) does the same by opening
  relations in its tree):
  `customer c ON c.id = o.customer_id` + newline + `JOIN region r ON r.id = c.region_id`.
  **Ctrl-O** goes back.

## Panel

`sqlh panel` (Helix `space i t`) opens a side panel next to the Helix pane it is run from —
in [Herdr](https://herdr.dev), half of that pane, on the side `panel.placement` says
(`right`, `left`, `up`, `down`) — or focuses it when it is already open. It belongs to that
Helix: it closes when Helix exits. Set the cursor in Helix, go to the panel, pick an object,
press **Ctrl-Y**: it is inserted at Helix's cursor and the focus goes back to Helix.

```
⛁ pg_local › sdsai › cadetst  dev
 1 Connections  2 Objects  3 Statement  4 Queries  5 History
 q.sql:7 · after JOIN
▾ ▦ sds_data_raw sds
  ▾ Relations  4
    ▸ → ai_extraction_process  sds.ai_extraction_id
    ▸ ← batch_document  sds.id
  ▸ Indexes  1
    · id  bigint  PK
─ Describe │ DDL │ Sample ─────────────────
```

Five tabs, `1`–`5` (it starts on **Statement**; `sqlh tui` on **Objects**):

1. **Connections** — the configured connections as a table (connection, database, schema,
   env; the database and schema shrink, then go, in a narrow pane), the current one marked
   `●`; connections with a `group:` sit in a folder of that name. The
   preview shows a connection field by field (host, port, database, schema, user, password
   masked, options; connection strings taken apart). Enter connects (and shows its objects).
   `C` new, `E` edit, `D` duplicate, `X` delete (asks first; not the current one): a form
   that writes the connection to the config file the server reads — only that connection's
   lines change, comments and the rest stay — checked first; Ctrl-T tests the connection,
   Ctrl-S saves, Esc cancels. Environment, sample policy and driver are ←/→ choices. The
   server re-reads the file (as any server does when its config file changes); editing the
   current connection reconnects.
2. **Objects** — the current connection's schema: tables, views, materialized views,
   sequences, types; the other schemas of its `schemas:` list as folders below. A table opens
   to its relations, the views using it, its indexes, constraints and triggers, and its
   columns. A related table opens like any table, so the tree below a table is a **chain of
   joins**: Ctrl-Y on a table reached through relations inserts the whole chain after `JOIN`
   (`customer c ON c.id = o.customer_id` + `JOIN region r ON r.id = c.region_id`), when the
   chain's first table is in the statement. A chain never returns to its own tables (no
   loops: below `orders → customer`, customer's relations leave out `orders`).
3. **Statement** — follows the cursor: the tables of the statement you are editing (from your
   edits and from the requests Helix sends — completion, hover, …; `space i t` syncs it), open
   (`panel.statement: collapsed`: closed), their relationships first, written as the
   statement refers to them (`sds.input_id`). `/` finds among the statement's tables (name or
   alias) and stays here. `:` finds **join chains**: the tables chains of relationships reach
   from the statement's tables (at most `relations.chainDepth` joins, default 4; no table
   twice, never through another of the statement's tables), nearest first, with how many
   chains reach each and the shortest length (`3 chains · 2 joins`; at most
   `relations.chainLimit`, default 10: `10+ chains`). Different chains can mean different
   rows (through a batch table only the batched rows join), so Enter opens them one by one:
   the shortest first, its table selected and its rows marked; `n` / `N` go to the next /
   previous chain (closing the one before where it leaves the next one's path; `panel.chains:
   all` opens them all at once), Esc stops. Ctrl-Y on a chain's table inserts that chain as
   joins; on the search result, the shortest one. Once `H` has closed the tables, tables
   joining the statement later arrive closed too, until `L` opens them again.
4. **Queries** — the files of `tui.queriesDir` (default `queries`), newest first; Enter opens
   one in Helix, `c` creates the next `query_<N>.sql` (a `SELECT * FROM` of the highlighted
   table, if any). The preview shows the file (highlighted) and its last run, when the
   loaded history has it. `R` renames a file (relative to the folder; `.sql` added; a file
   open in the attached Helix is renamed there, with `:move`, so its buffer follows), `X`
   deletes one (asks first; not while it is open in Helix).
5. **History** — **This statement**: the versions of the statement you are editing (earlier
   runs of a similar query in the same file), newest first, the current text marked; then the
   workspace's recent queries on the current connection (`c`: all connections), each opening
   (Enter) to its versions. The preview's **Query**
   tab shows the run (when, connection, rows, time, error, result file) and its SQL; **Diff**
   compares it with the statement at the cursor (a version of it), else with today's version
   in its file (the statement most like it, from Helix's buffer); `.` `r` **replaces the
   statement in the buffer** with a version (one undo step). `o` opens a run's result in
   `results.viewer` (it must open its own window or pane, like the Herdr pane script), `e` its
   file — recreated (its connection header and the run's SQL) when it is gone.

Keys (all configurable, see below):

- **Enter** navigates: opens and closes, connects, opens a file. It never inserts.
- **Ctrl-Y** inserts: a table (after `JOIN` with the alias and `ON` condition, or the
  chain), a column (`alias.col` when its table is in the statement; Space marks several:
  `o.id, o.total`), an object's name, a query. With the cursor on a table name it is copied to
  the clipboard instead. Without an editor (`sqlh tui` alone) it copies, and says so.
- `.` (or `a`) — the actions of the selection, each with a letter: for a table **insert** (as
  Ctrl-Y), **insert the name**, **insert `JOIN … ON …`** (`J`: after `JOIN` the clause,
  elsewhere with the keyword on its own line; an error when the table relates to none of the
  statement's), `SELECT * FROM` /
  all columns, copy, a new query file, the DDL, a sample; for connections connect, edit,
  duplicate, delete; for runs replace, insert, copy, open.
- `/` — search, like an omni bar: it finds what the tab lists (connections in Connections,
  queries in Queries, runs in History, the statement's tables in Statement, tables and views
  in Objects — all of them for an empty query); a first character picks another kind: `:`
  tables and views (in Statement: join chains, see above), `.` columns (`.cust`, or `.orders.tot` for table.column), `#` indexes,
  `!` constraints, `^` triggers, `~` sequences, `@` connections, `*` everything. While the
  query is empty the preview lists these; Tab / Shift-Tab switch the kind (rewriting the
  prefix). Enter goes to the result (in its tab), Ctrl-Y inserts it.
- Tab / Shift-Tab switch the preview's tabs, which depend on the selection: a table or view
  has **Describe** (columns, relationships, a view's definition), **DDL** (as go to
  definition shows it) and **Sample**; a column Describe and Sample; an index, constraint,
  sequence Describe; a connection, a query file one tab each; a run **Query** and **Diff**.
  The tab chosen is kept for that kind of selection (DDL stays DDL from table to table).
  J/K scroll the preview. SQL is highlighted in the colours of Helix's `modus_vivendi_tinted`
  theme; `panel.syntax.<class>: "#rrggbb"` changes them (`keyword`, `comment`, `string`,
  `number`, `function`, `type`, `alias`, `parameter`, `operator`).
- `H` closes the deepest open level of the tab's tree (open relations, then the tables, then
  the groups; again for the next level), `L` (Statement) opens the next level, `z` scrolls
  the selected row to the top (vim's `zt`).
- `c` creates a query file (`n` / `N` are the join chains' in Statement).
- `ctrl+:` opens the config file the server reads in `$VISUAL` / `$EDITOR` (else `vi`), in
  the panel's pane, and reloads it when you quit the editor; `ctrl+R` reloads it (after
  editing it elsewhere). The panel takes the new keys, icons, colours and statement setting
  at once; the server its connections and the rest. A broken file is refused: the error stays
  on screen until a key, the old config stays.
- **Sample** shows one row of the table as `column  value`, wrapped to fit; `]`/`[` the
  next/previous of up to 50 fetched. On a column, `s` counts its most common values. Samples
  run on a pooled session of their own (never the editor's: they neither wait for nor block a
  running query), with a 10 s timeout, long cells cut; they are dropped after a query runs.
  Per connection `sample: auto | manual | off`; the default follows `environment`: automatic
  for tables on `dev`/`test` (once the selection rests; views only on request, they may be
  expensive queries), on request (`s`) on `prod`.
- `?` — the keys of the tab, as a searchable list: the tab's own actions first, then
  navigation, the preview, the rest. Typing filters the descriptions (`@` + a key filters on
  the keys); Enter runs the action, Esc clears the filter, then closes.
- `y` copies the name, `r` refreshes the schema cache, `q` quits; Backspace (`backToEditor`)
  focuses the panel's Helix pane (Herdr's `prefix space o` goes to the previous pane).

Actions belong to tabs: the Connections tab has `newConnection` … `deleteConnection`;
Objects and Statement `insert`, `mark`, `sample`, `nextRow`, `prevRow`, `newQuery`, `refresh`
(Statement also `expandAll`, `nextChain`, `prevChain`, `clearChains`); Queries `newQuery`, `renameQuery`, `deleteQuery`; History
`insert`, `openResult`, `editFile`, `historyAllConns`; the rest are in every tab. A key of
another tab says so in the footer.

`panel.keys.<action>: key` (or a list) in the config replaces an action's keys in every tab;
`panel.keys.<tab>.<action>` (tabs `connections`, `objects`, `statement`, `queries`, `history`;
also `search`, `form`) in one tab, so a key may mean different things in different tabs:

```yaml
panel:
  keys:
    insert: i                # every tab
    connections:
      duplicateConnection: d
    objects:
      sample: d
```

The defaults:

| actions | default |
|---|---|
| `up` `down` `top` `bottom` `pageUp` `pageDown` `scrollTop` | `k ↑ ctrl+p`, `j ↓ ctrl+n`, `g home`, `G end`, `ctrl+u pgup`, `ctrl+d pgdown`, `z` |
| `expand` `collapse` `collapseAll` `expandAll` `open` `mark` | `l →`, `h ←`, `H`, `L`, `enter`, `space` |
| `insert` `copy` `actions` `search` | `ctrl+y`, `y`, `. a`, `/` |
| `previewNext` `previewPrev` `previewDown` `previewUp` | `tab`, `shift+tab`, `J`, `K` |
| `sample` `nextRow` `prevRow` | `s`, `]`, `[` |
| `newQuery` `renameQuery` `deleteQuery` | `c`, `R`, `X` |
| `nextChain` `prevChain` `clearChains` (Statement) | `n`, `N`, `esc` |
| `openResult` `editFile` `historyAllConns` | `o`, `e`, `c` |
| `newConnection` `editConnection` `duplicateConnection` `deleteConnection` | `C`, `E`, `D`, `X` |
| `refresh` `help` `quit` `backToEditor` | `r`, `?`, `q`, `backspace` |
| `editConfig` `reloadConfig` | `ctrl+:`, `ctrl+R` |
| `tab1` … `tab5` | `1` … `5` |
| `searchUp` `searchDown` `searchOpen` `searchInsert` `searchCancel` `searchNextKind` `searchPrevKind` | `↑ ctrl+p ctrl+k`, `↓ ctrl+n ctrl+j`, `enter`, `ctrl+y`, `esc`, `tab`, `shift+tab` |
| `formSave` `formTest` `formCancel` `formNext` `formPrev` | `ctrl+s`, `ctrl+t`, `esc`, `tab ↓`, `shift+tab ↑` |

Key names are Bubble Tea's (`ctrl+y`, `shift+tab`, `backspace`, `space`, `G`, `]`). With
shift, either spelling works: `ctrl+R` or `ctrl+shift+r`, `ctrl+:` or `ctrl+shift+;` (US
layout) — the panel reads the kitty keyboard protocol (Bubble Tea v2), which Ghostty and
Herdr speak. In a terminal without it (macOS Terminal, tmux without `extended-keys`)
ctrl+shift combinations don't arrive: `?` lists every action and runs it. A key on two
actions of one tab (or of the search, or the form), an unknown action or an action a tab
doesn't have are config errors.

How Ctrl-Y reaches Helix (which has no API for it): the panel stores the choice in the server
and sends Helix `panel.insertKeys` (default `esc space i P`); that binding runs
`:insert-output sqlh panel take …`, which turns the choice into text for Helix's cursor *at
that moment* (one undo step). Each Helix pane has its own panel and its own pending choice.

The panel pane is started through the shell startup hook of `~/.config/helix`
(`HELIX_HELPER_ARGV` and `HELIX_ORIGIN_PANE_ID` in the new pane's environment); without that
hook `exec sqlh panel run …` is typed into the new shell. `SQLH_NO_FOCUS=1` keeps the focus
where it is (scripted tests). While the panel (or `sqlh tui`) runs, the log goes to
`--log FILE`, else `tui.log` in sqlh's cache directory: nothing is written over the screen.

## sqlh tui

`sqlh tui` is the [panel](#panel) as an app, started from a shell (in Herdr) rather than from
Helix; it starts on **Objects**.

- `c` creates the next `query_<N>.sql` in `tui.queriesDir` (N = the highest existing number + 1,
  from 1; renamed or moved files are yours) with the connection header, and a `SELECT * FROM`
  of the highlighted table, if any; it opens in Helix (`tui.editor`, default `hx`) in a new
  pane next to the app (half of it, opposite `panel.placement`), and the app becomes that
  Helix's panel: Ctrl-Y inserts at its cursor, `space i t` there focuses the app.
- Enter on a query file (or `e` on a run in History) opens it in that Helix, or in a new one.
- When that Helix exits the app stays, without an editor (Ctrl-Y then copies).

Without an editor's server for the workspace the app runs one itself (headless: no editor,
the same catalog, runs and history); when Helix starts there, its server takes over the socket
and the app follows it, and back when Helix exits. Helix stops its language servers when it
quits, so the editor's sqlh server ends with it; the app's own server stays connected (the
database and the schema cache, one idle session) while it gives way, and serves again at once
— the config re-read if it changed, the workspace's connection followed, the cache refreshed
in the background if DDL ran meanwhile. The app keeps its trees through the switch.

## History

Every statement sqlh runs — from the editor (`space i r/e/d`, code actions), `sqlh run`/`export`
through the editor's server, standalone runs and `sqlh dump` — is appended to
`{workspace}/.sqlh/history.jsonl` (next to the results, gitignored): time, kind, file and line,
the SQL, connection, database, schema, environment, rows, time taken, status or error, and the
result file. Runs of the same file whose SQL is similar enough (60 % of the tokens in common,
comments, whitespace and case aside) to the latest version of an earlier query continue it, as
its next version; the [panel](#panel) shows them. Nothing is deleted.

## What `run` / `export` execute

In order of precedence:

1. `--sql 'select …'` (or `--sql -`)
2. piped stdin, if longer than one character — Helix `:pipe-to` sends the selection, which in
   normal mode is only the character under the cursor, so that is ignored and…
3. `--file F --line L [--col C]`: the statement at that line (1-based, e.g. Helix
   `%{cursor_line}`); `--file` without `--line` runs the whole file. With a server, an open
   file's live buffer is used.

Options: `--out PATH` (CSV path; further result sets `<name>_2.csv`, …), `--table` (print a text table instead
of writing CSV), `--json` (structured result), `--conn ALIAS` / `--standalone` (run on a separate
connection, leaving the editor's alone), `-C DIR` (find the workspace from `DIR`), `--open` (open
the CSV in the [viewer](#viewer) when done).

`--limit ask` (`run`) picks the row limit: a preset, a typed number (Enter takes `10` as
typed, not the `100` it matches), or `all`; Esc cancels.

`--bg` starts the same command again in a detached process (its own session, no terminal, stdio
on `/dev/null`) and returns at once; the query reports through the server (status line, `sqlh
cancel`). Piped SQL is passed on; `--table`/`--json` can't be used with it.

Ctrl-C cancels the query, also when it runs in the server.

## Viewer

`sqlh open` and `--open` hand result files to `results.viewer`: a program and its arguments,
`"{}"` standing for the file(s) (else they are appended), `~` expanded in the program. Default:
`open` (macOS) / `xdg-open`. The editor's server's configuration is used, else the config file.

```yaml
results:
  viewer: ["vd"]                                                       # from a terminal
  viewer: ["~/.config/helix/open-in-herdr-pane.nu", "0.8", "vd", "{}"] # VisiData in a Herdr pane
```

From a terminal the viewer inherits it; otherwise (`--editor`, `--bg`, a pipe) sqlh waits for
it to start and reports its output if it fails.

## Editor mode

`--editor` (on `run`, `export`, `dump`, `open`, `connect`, `db`, `cancel`, `refresh` and the
catalog commands) says the command was started from an editor keybinding rather than a
terminal:

- **errors go to the editor's status line** (through the workspace's server), **never to
  stderr**, and the exit status is 1. Helix 25.07 takes a failed command's stderr as its
  *output*: `:pipe-to` discards it, `:insert-output` inserts it into the buffer. Without a
  server Helix just reports "Shell command failed: status 1";
- summaries and confirmations aren't printed either: the server shows them;
- **stdin is ignored**: `:pipe-to` sends the selection — whatever the last motion selected —
  so `run --file F --line L` always runs the statement at the line (`space a` runs a
  selection).

`--file F` (Helix `%{buffer_name}`) finds the workspace from F's directory — also on
`latest`, `open`, `status`, `cancel`, `refresh` and the catalog commands — falling back to the
current directory for scratch buffers.

## Query files: connection header and dump

A query file says which connection it runs on in a header among its leading comments:

```sql
-- sqlh: conn=pg_local

select …
```

- **The editor keeps it up to date.** Whenever a statement of an open file runs in the
  editor's server (code actions, `space i r/e/d`, `sqlh run --file`), and when you switch
  connection from it (`space i C`, `sqlh connect --file F`), the server rewrites the header —
  or inserts it at the top — in the **buffer** (`workspace/applyEdit`; the buffer becomes
  modified, save it for the CLI to see it). Files that aren't open are never edited.
- `db=` and `schema=` (from earlier versions) are an **error**: a connection is one database
  and schema, so make a connection for it and name it with `conn=`. Such a file doesn't run
  (editor or `dump`) and its header isn't rewritten until it is fixed.
- Edit it by hand to point the file somewhere else; other `key=value` pairs are kept.
- **When the header and the editor differ** (you switched from another buffer, or opened a
  file last run elsewhere), the header line gets an information diagnostic — "this file
  targets oracle_dev, the editor is on pg_local" — and the code actions (`space a`; first when
  the cursor is on the header) offer **Switch the editor to oracle_dev** (`sqlh.useHeader`) and **Point the file's header at pg_local** (rewrites it without
  running; `sqlh.stampHeader`). A connection that isn't configured is a warning. These checks
  stay on with `diagnostics: false`.
- Session state set by statements (`SET search_path`, `ALTER SESSION`, temp tables, an open
  transaction) is not carried over to `dump`: put what the query needs in the file, or use a
  connection set to that schema (`schema:`).

`sqlh dump FILE… [-d DIR | -o FILE] [--line L [--col C]] [--conn ALIAS]` runs each file on its
header's connection (`--conn` overrides; neither → that file fails) on a new connection and
writes all rows to `DIR/<file>.csv` (default: the current directory; further result sets
`<file>_2.csv`, …), replacing it only once complete. `--line` runs just the statement at that
line. `latest.csv` and the results directory are not touched. Several files run independently:
the others continue when one fails, and the exit status is 1 at the end. CSV paths go to
stdout, summaries and errors to stderr.

```sh
sqlh dump reports/monthly.sql                    # → ./monthly.csv
sqlh dump reports/*.sql -d ~/exports/$(date +%F) # one CSV per file, each on its own connection
sqlh dump report.sql --conn pg_test -o /tmp/r.csv
```

From Helix, `space i d` runs `sqlh dump --editor --bg FILE --line L`: the statement at the
cursor, in the editor's server — its connection (the header follows), the unsaved buffer,
progress, summary and errors in the status line, `space i X` cancels — into Helix's working
directory. Without a server it falls back to the terminal behaviour.

**Reuse.** If the same statement (exact text; comments above it, like the header, don't count)
already produced a complete result in the results directory — an export (`space i e`), or a
preview (`space i r`) that returned fewer rows than the limit — `space i d` copies that CSV
instead of running the query again (`… · reused the result from 14:02 (3m ago)`). Only while:

- same connection and database, same editor session: any statement that isn't a plain read
  (DML, DDL, PL/SQL, `BEGIN`/`COMMIT`, `SELECT … FOR UPDATE`, `nextval`, …), a reconnect and
  a connection or database switch forget all results;
- the result is younger than `results.reuseFor` (default `10m`; `0` disables);
- the CSV still exists.

Writes by other sessions, functions that write, and time-dependent queries (`now()`) are not
detected: that's what the age limit and the status line are for. `--fresh` always runs; dumps
from the terminal never reuse.

## Examples

```sh
sqlh run --sql "select * from caproduct" --limit 500     # → .sqlh/results/oracle_dev_….csv
sqlh export -f report.sql -l 12 && vd "$(sqlh latest)"
echo "select count(*) from caproduct" | sqlh run --table
sqlh export --conn pg_local --sql "select * from batch" --out /tmp/batch.csv
sqlh connect pg_local
```

## Helix

Keybindings under `space i` (in `~/.config/helix/config.toml`) call the CLI directly, in
[editor mode](#editor-mode). Like code actions, they report in the status line, not popups:
queries start with `--bg`, so `:pipe-to` returns at once, and the server shows progress, the
summary or the error in the editor.

```toml
[keys.normal.space.i]
r = ":pipe-to sqlh run --editor --bg --open -f r##'%{buffer_name}'## -l %{cursor_line}"
e = ":pipe-to sqlh export --editor --bg --open -f r##'%{buffer_name}'## -l %{cursor_line}"
d = ":pipe-to sqlh dump --editor --bg r##'%{buffer_name}'## -l %{cursor_line}"
o = ":pipe-to sqlh open --editor -f r##'%{buffer_name}'##"
t = ":pipe-to sqlh panel --editor -f r##'%{buffer_name}'## -l %{cursor_line} --col %{cursor_column}"
P = ":insert-output sqlh panel take --editor -f r##'%{buffer_name}'## -l %{cursor_line} --col %{cursor_column}"
T = [":insert-output sqlh tables -p --editor -f r##'%{buffer_name}'## -l %{cursor_line} --col %{cursor_column}", ":redraw"]
f = ":format"
C = [":pipe-to sqlh connect --editor -f r##'%{buffer_name}'##", ":redraw"]
s = ":sh sqlh status -f r##'%{buffer_name}'##"
X = ":lsp-workspace-command sqlh.cancel"
U = ":lsp-workspace-command sqlh.refresh"
```

These are for `shell = ["nu", …]` in Helix's config (`r##'…'##` is a Nushell raw string, so any
path survives); with the default `sh`, write `'%{buffer_name}'`. `r`/`e` open the result in the
[viewer](#viewer).

| Keys | |
|---|---|
| `space i r` / `e` | run the statement at the cursor → CSV, first `results.previewLimit` rows / all rows, opened in the viewer |
| `space i d` | dump the statement at the cursor → `<file>.csv` in Helix's working directory (all rows; copies a recent complete result of the same statement, see [Query files](#query-files-connection-header-and-dump)) |
| `space i o` | open the latest result in the viewer |
| `space i t` | open or focus the [panel](#panel) (synced to the cursor) |
| `space i P` | (pressed by the panel) insert what the panel chose at the cursor |
| `space i T` | insert a table with the inline picker (related first; after `JOIN` the join clause), see [Table picker](#table-picker) |
| `space i f` | format the file (`:format`) |
| `space i C` | switch connection: the connections table (connection, database, schema, env), filtered as you type; the buffer's connection header follows |
| `space i s` | status: connection, schema, running query, results directory (popup, Esc closes) |
| `space i X` | cancel the running query (`:lsp-workspace-command sqlh.cancel`) |
| `space i U` | reload schema metadata (`:lsp-workspace-command sqlh.refresh`; runs that execute DDL reload it themselves) |

Not bound, but there: `run --limit ask` (pick a row limit — preset, typed number or `all` —
then run), `run` without `--open` (CSV only), `dump --fresh` (never reuse) and
`[":insert-output sqlh columns -p --editor -f r##'%{buffer_name}'## --prefix p.", ":redraw"]`
(multi-pick columns).

`space a` (code actions) does the same per statement.

Notes:

- `:sh` shows output in a popup that only Esc/Ctrl-C closes, and sets "Command run" over any
  status message, so queries aren't run from `:sh`.
- Pickers need the terminal, so they run under `:pipe-to`/`:insert-output` (they draw on
  `/dev/tty`), followed by `:redraw`. The panel runs in its own pane: `space i t` needs no
  `:redraw`.

## Files

- sockets: `~/Library/Caches/sqlh/<hash>.sock` (macOS), `$XDG_RUNTIME_DIR/sqlh/` (Linux);
  override with `SQLH_RUNTIME_DIR`, or point the CLI at a socket with `SQLH_SOCK`
- remembered connection per workspace: `~/.local/state/sqlh/state.json` (`$XDG_STATE_HOME`)
- the workspace is the editor's project root; the CLI finds it by walking up from `--file`'s
  directory (or the current directory). Standalone, the root is the nearest ancestor with
  `.git`/`.helix`/`.jj`/`.hg`/`.svn`.
