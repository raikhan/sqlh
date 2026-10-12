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
| `sqlh tables [-s SCHEMA] [--all]` | tables/views as you'd type them (default schema unqualified, others `schema.table`, quoted when needed); `-p` picks one with a columns preview |
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
`~/.config/helix/sqlh.nu`, which wraps these commands so that queries run from an async `:sh`
(Helix stays usable, `space i x` cancels) and every outcome shows in Helix's popup:

| Keys | |
|---|---|
| `space i r` / `v` / `e` | run the statement at the cursor → CSV (preview) / → VisiData pane / export all rows |
| `space i l` | pick a row limit (preset, typed number, `all`), then run |
| `space i o` | open the latest result in VisiData (Herdr pane) |
| `space i x` | cancel the running query |
| `space i t` / `c` | insert a table / columns (fzf) |
| `space i C` / `D` | switch connection / database (fzf) |
| `space i s` / `u` | status / reload schema metadata |

`space a` (code actions) does the same per statement, without the helper. The raw form:

```
:sh sqlh run --limit 500 --file %{buffer_name} --line %{cursor_line}
```

(`:sh` shows only stdout on success, and `sqlh run` prints the summary on stderr — hence the
helper. Pickers need the terminal, so they run under `:pipe-to`/`:insert-output`, followed
by `:redraw`.)

## Files

- sockets: `~/Library/Caches/sqlh/<hash>.sock` (macOS), `$XDG_RUNTIME_DIR/sqlh/` (Linux);
  override with `SQLH_RUNTIME_DIR`, or point the CLI at a socket with `SQLH_SOCK`
- remembered connection per workspace: `~/.local/state/sqlh/state.json` (`$XDG_STATE_HOME`)
- the workspace is the editor's project root; the CLI finds it by walking up from `--file`'s
  directory (or the current directory). Standalone, the root is the nearest ancestor with
  `.git`/`.helix`/`.jj`/`.hg`/`.svn`.
