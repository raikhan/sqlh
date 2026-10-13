# sqlh CLI

`sqlh` with no command (or `sqlh lsp`) is the language server. The other commands work
with the server of the current workspace — the same connection, session and **unsaved
editor buffers** — via a Unix socket. Without a running server, `run`, `export`,
`connections` and `latest` work standalone from the config.

| Command | What it does |
|---|---|
| `sqlh run` | run SQL → CSV, first N rows (`--limit`, default `results.previewLimit`); prints the CSV path(s) on stdout, summary on stderr |
| `sqlh export` | same, all rows |
| `sqlh connect [alias]` | switch the editor's connection (no alias: pick with fzf); remembered per workspace |
| `sqlh connections` / `ls` | list connections, `*` = current |
| `sqlh status [--all]` | workspace, connection, running query, results dir (`--all`: all servers) |
| `sqlh cancel` | cancel the running query |
| `sqlh refresh` | reload schema metadata (after DDL) |
| `sqlh latest` | path of the newest result CSV: `vd $(sqlh latest)` |
| `sqlh tables [-s SCHEMA] [--all]` | tables/views as you'd type them (default schema unqualified, others `schema.table`, quoted when needed); `-p` picks one (preview: `describe`); with `--file F --line L --col C` the tables related to the statement's come first and after `JOIN` it inserts the join clause, see [Table picker](#table-picker) |
| `sqlh describe TABLE` | columns (foreign keys marked), relationships, a view's definition (`--json`) |
| `sqlh relations TABLE` | the tables and views related to TABLE, grouped (`--long`, `--json`; `-p` picks one) |
| `sqlh columns [TABLE] [--prefix p.] [--sep ", "]` | a table's columns; `-p` multi-picks (Tab, Ctrl-A all) → `p.a, p.b`; no TABLE with `-p`: pick the table first |
| `sqlh schemas [--all]` / `sqlh databases` | lists; `-p` picks one |
| `sqlh db [NAME]` | switch the editor's connection to another database (PostgreSQL; no NAME: pick) |

## Pickers

`-p` runs fzf (`--height=80%`, kitty keyboard protocol switched off around it, like
`fzf-in-helix.nu`) and prints the choice **without a trailing newline** when stdout is not a
terminal, so it can be inserted directly from Helix:

```
:insert-output sqlh tables -p
:insert-output sqlh columns -p --prefix p.
```

Esc/no match prints nothing (exit 0). Without `-p` the commands print one insertable name per
line (`--long` for details, `--json` structured), e.g. to feed another picker.
`SQLH_FZF_ARGS` adds fzf arguments.

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
- typing ranks by match quality (only the names are matched);
- after `JOIN`, Enter inserts `customer c ON c.id = o.customer_id` (alias from the table's
  initials, numbered when taken; keyword case from `lowercaseKeywords`); elsewhere, or with
  `relations.insert: name` / `--insert name`, the name. **Ctrl-T** inserts the other one;
- **Ctrl-R** lists the highlighted table's relationships (prompt `o › customer`), and
  accepting there inserts the whole chain:
  `customer c ON c.id = o.customer_id` + newline + `JOIN region r ON r.id = c.region_id`.
  **Ctrl-O** goes back.

## What `run` / `export` execute

In order of precedence:

1. `--sql 'select …'` (or `--sql -`)
2. piped stdin, if longer than one character — Helix `:pipe-to` sends the selection, which in
   normal mode is only the character under the cursor, so that is ignored and…
3. `--file F --line L [--col C]`: the statement at that line (1-based, e.g. Helix
   `%{cursor_line}`); `--file` without `--line` runs the whole file. With a server, an open
   file's live buffer is used.

Options: `--out PATH` (CSV path for the first result set), `--table` (print a text table instead
of writing CSV), `--json` (structured result), `--conn ALIAS` / `--standalone` (run on a separate
connection, leaving the editor's alone), `-C DIR` (find the workspace from `DIR`).

Ctrl-C cancels the query, also when it runs in the server.

## Examples

```sh
sqlh run --sql "select * from caproduct" --limit 500     # → .sqlh/results/oracle_dev_….csv
sqlh export -f report.sql -l 12 && vd "$(sqlh latest)"
echo "select count(*) from caproduct" | sqlh run --table
sqlh export --conn pg_local --sql "select * from batch" --out /tmp/batch.csv
sqlh connect pg_local
```

## Helix

Keybindings under `space i` (in `~/.config/helix/config.toml`) call the helper
`~/.config/helix/sqlh.nu`. Like code actions, they report in the status line, not popups:
queries start in a detached process, so `:pipe-to` returns at once, and the server shows
progress, the summary or the error in the editor.

| Keys | |
|---|---|
| `space i r` / `v` / `e` | run the statement at the cursor → CSV (preview) / → VisiData pane / export all rows |
| `space i l` | pick a row limit (preset, typed number, `all`), then run |
| `space i o` | open the latest result in VisiData (Herdr pane) |
| `space i x` | cancel the running query (`:lsp-workspace-command sqlh.cancel`) |
| `space i t` / `c` | insert a table (related first; after `JOIN` the join clause) / columns (fzf) |
| `space i C` / `D` | switch connection / database (fzf) |
| `space i s` / `u` | status (popup) / reload schema metadata (`sqlh.refresh`) |

`space a` (code actions) does the same per statement, without the helper.

Why the helper:

- Helix 25.07 handles a failed shell command's stderr as its *output*: `:pipe-to` discards
  it, `:insert-output` inserts it into the buffer. The helper exits with empty stderr and
  relays errors with `sqlh notify [--error] MESSAGE` (hidden command: shows MESSAGE in the
  editor's status line via the server).
- `:sh` shows output in a popup that only Esc/Ctrl-C closes, and sets "Command run" over any
  status message, so queries aren't run from `:sh`.
- Pickers need the terminal, so they run under `:pipe-to`/`:insert-output`, followed by
  `:redraw`.

## Files

- sockets: `~/Library/Caches/sqlh/<hash>.sock` (macOS), `$XDG_RUNTIME_DIR/sqlh/` (Linux);
  override with `SQLH_RUNTIME_DIR`, or point the CLI at a socket with `SQLH_SOCK`
- remembered connection per workspace: `~/.local/state/sqlh/state.json` (`$XDG_STATE_HOME`)
- the workspace is the editor's project root; the CLI finds it by walking up from `--file`'s
  directory (or the current directory). Standalone, the root is the nearest ancestor with
  `.git`/`.helix`/`.jj`/`.hg`/`.svn`.
