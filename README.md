# sqlh: SQL Language Server + query runner

![test](https://github.com/raikhan/sqlh/workflows/test/badge.svg)

An implementation of the Language Server Protocol for SQL, plus a CLI and a terminal UI for
running queries, built around [Helix](https://helix-editor.com) (and [Herdr](https://herdr.dev)
panes) but usable from any LSP client.

sqlh is a fork of [sqls](https://github.com/sqls-server/sqls) (MIT). The parser, completer,
hover and formatter come from sqls; the fork adds the rest.

## Note

This project is currently under development and there is no stable release. Therefore, destructive interface changes and configuration changes are expected.

## What this fork adds

Compared to upstream sqls:

- **Oracle with a pure-Go driver** ([go-ora](https://github.com/sijms/go-ora), no Oracle
  Instant Client needed), with schema allow-lists for big dictionaries, views, synonyms,
  packages and PL/SQL blocks.
- **Running queries to CSV**: preview (first N rows) or full export, streamed to
  `{workspace}/.sqlh/results/`, cancellable, reported in the editor's status line rather than
  in popups. Runs the statement at the cursor or every statement in a selection. See
  [Running queries](#running-queries).
- **The `sqlh` CLI**: it talks to the editor's server over a Unix socket (same connection and
  same unsaved buffers), or runs standalone: `run`, `export`, `dump`, `connect`, `tables`,
  `describe`, `ddl`, `relations`, `columns`, `keys`, … See [doc/cli.md](doc/cli.md).
- **A side panel / app** (Bubble Tea TUI): connections, schema objects, the statement at the
  cursor, query files and history. It inserts tables, join chains and columns at Helix's
  cursor. See [Side panel](#side-panel).
- **Schema-aware diagnostics** for DML and DDL, checked against the schema cache only, with no
  database round-trips.
- **Deeper completion**: CTEs, derived tables, correlated subqueries, `RETURNING`/`ON
  CONFLICT`/`MERGE`, DDL statements, function names with signatures (from `pg_proc` / Oracle
  packages), and **JSON keys** sampled from the data (PostgreSQL `->`/`->>`/`#>`/jsonpath,
  Oracle `json_value`/`json_query`/…).
- **Relationships**: foreign keys, keys inherited through views and guessed ("maybe")
  relationships. These drive join completion, hover, the table picker and join-chain search.
- **Go to definition** of tables and views (DDL fetched or generated from the catalog), of
  aliases and of CTEs. **Rename** works on table aliases.
- **Query files** with a `-- sqlh: conn=ALIAS` header, `sqlh dump` for scheduled exports,
  reuse of recent results, and a **query history** with versions and diffs.
- **Per-connection current schema**, `environment` labels (dev/test/prod), and secrets from
  `passwdCmd` (e.g. the macOS keychain) or `${ENV}` references.
- A formatter that is safe for format-on-save (JSON operators, DDL layout).
- The language server is started as `sqlh lsp`; `sqlh` on its own starts the app.

The working plan and progress log is in [PLAN.md](PLAN.md).

## Features

sqlh aims to provide advanced intelligence for you to edit sql in your own editor.

### Support RDBMS

- MySQL([Go-MySQL-Driver](https://github.com/go-sql-driver/mysql))
- PostgreSQL([pgx](https://github.com/jackc/pgx))
- Oracle([go-ora](https://github.com/sijms/go-ora), pure Go)
- SQLite3([go-sqlite3](https://github.com/mattn/go-sqlite3))
- MSSQL([go-mssqldb](https://github.com/microsoft/go-mssqldb))
- H2([h2go](https://github.com/CodinGame/h2go))
- Vertica([vertica-sql-go](https://github.com/vertica/vertica-sql-go))
- ClickHouse([clickhouse-go](https://github.com/ClickHouse/clickhouse-go))

PostgreSQL, Oracle and SQLite get the most attention in this fork: function catalogs,
column types, view lineage, DDL and JSON completion. The other drivers keep sqls' feature set.

### Language Server Features

#### Diagnostics

Obvious mistakes are underlined as you type, checked against the cached schema (no queries):
an alias or table that isn't in the statement (`fm.*` with only `fd` in FROM), unknown tables
and columns, with "did you mean" suggestions. When sqlh can't be sure (PL/SQL, dynamic SQL,
templates, synonyms not loaded yet, …) it says nothing. Writing to a view the database can't
write through is flagged too (`UPDATE v_batch_document …`: "a read-only view"; `DELETE FROM` a
materialized view), from the database's own catalog (PostgreSQL `pg_relation_is_updatable`,
Oracle `ALL_UPDATABLE_COLUMNS`, INSTEAD OF triggers and rules included). Turn off with
`diagnostics: false`.

DDL is checked too, statement by statement in document order: unknown tables in
`ALTER`/`DROP` (not with `IF EXISTS`)/`TRUNCATE`/`CREATE INDEX … ON`/`REFERENCES`/`COMMENT ON`,
`DROP VIEW` on a table (and the other wrong-kind drops), unknown columns in `DROP`/`RENAME`/
`ALTER COLUMN`, index and key column lists, `REFERENCES t (col)`, `ADD COLUMN` of a column that
exists, unknown indexes, constraints, sequences and triggers, key columns a `CREATE TABLE`
doesn't define, and the query of `CREATE VIEW … AS`. Objects created, altered or dropped by
earlier statements of the file count (also `EXECUTE IMMEDIATE '<DDL>'` in PL/SQL blocks), and a
DDL statement that ran successfully isn't checked again (`DROP TABLE t` would otherwise turn red
once `t` is gone). `INSERT`/`UPDATE`/`DELETE … RETURNING`, `ON CONFLICT` and `MERGE` columns are
checked against the target table.

#### Auto Completion

![completion](./imgs/sqls-completion.gif)

- DML(Data Manipulation Language)
  - [x] SELECT
    - [x] Sub Query (derived tables: `s.` lists the subquery's columns, incl. aliased
          expressions such as `count(*) as n`)
    - [x] WITH queries (CTEs): names in `FROM`/`JOIN` (listed first), `x.` and unqualified
          columns, explicit column lists `x (a, b)`, CTEs built on CTEs, `RECURSIVE`
    - [x] Scoping per query level: an inner subquery's or CTE body's `FROM` doesn't leak
          into the outer query; `UNION` branches are separate; correlated subqueries see
          the enclosing query's aliases (`b.` inside `EXISTS (…)`)
  - [x] INSERT (incl. `RETURNING`, `ON CONFLICT (…) DO UPDATE SET` with `EXCLUDED.`)
  - [x] UPDATE, DELETE (incl. `RETURNING`)
  - [x] MERGE (`INTO`/`USING` tables, `UPDATE SET`/`INSERT (…)` target columns)
- DDL(Data Definition Language): the keywords that fit the position, then names
  - [x] CREATE TABLE: column types of the dialect (PostgreSQL, Oracle, SQLite, a generic set
        for the others; user types and domains too), column and table constraints,
        `REFERENCES` tables (the one guessed from the column name first, `customer_id` →
        `customer`) and their columns (keys first), key column lists from the columns
        defined above, `CHECK`/`DEFAULT` expressions; `… AS SELECT` like a query
  - [x] ALTER TABLE: tables, actions, columns of the table (`DROP`/`RENAME`/`ALTER`/`MODIFY`
        `COLUMN`), types after `ADD [COLUMN] x`/`TYPE`, constraints, triggers
  - [x] DROP / TRUNCATE / COMMENT ON / GRANT … ON / ALTER VIEW|INDEX|SEQUENCE|TRIGGER: only
        the kind of object named (`DROP VIEW` → views, `DROP INDEX` → indexes, `DROP
          SEQUENCE`, `DROP TRIGGER`, `DROP TYPE`), `schema.` qualified, `COMMENT ON COLUMN t.`
  - [x] CREATE INDEX (`ON` table, its columns), CREATE VIEW (`AS SELECT` like a query),
        CREATE TRIGGER (`ON` table), CREATE SEQUENCE options

  Index, constraint, sequence, trigger and type names come from the database's catalog
  with the background load (Oracle: the allowed schemas; a non-owner login may not see
  another owner's triggers and sequences).

  A space triggers completion where names are expected (after `DROP VIEW`, `ALTER TABLE`,
  `REFERENCES`, `FROM`, `JOIN`, …), so the list comes up without typing a first letter;
  at other spaces the server answers with nothing (no popup).

  The schema cache is reloaded after a run that executed DDL (`refreshAfterDDL: false` to
  turn off; `space i U` / `sqlh refresh` still reload by hand).

DDL is formatted with its own layout: a `CREATE TABLE`'s columns one per line, an `ALTER TABLE`
with several actions one action per line, other DDL on one line, the query of `CREATE VIEW … AS`
below its header.

#### JSON keys

Inside JSON paths, completion offers the keys found in the data. sqlh samples 1,000 non-null
rows and caches the result for 10 minutes; a lookup that takes longer than 1.5 s finishes in
the background.

- PostgreSQL `json`/`jsonb`: `col->'…'`, `->>'…'`, nested chains including array indexes,
  `#>`/`#>>` `'{a,…'`, `?`, `jsonb_path_*(col, '$.a.…')`. Each key comes with its types and
  frequency (`code: string|null · 6/6 sampled`).
- Oracle: `json_value`/`json_query`/`json_exists`/`json_table(col, '$.…')` on any column, via
  `JSON_DATAGUIDE`. Keys that need quotes are inserted as `."CAS RN"`.

#### Functions

Typing a prefix offers functions with their signature and description; accepting inserts
`name()` / `name($0)` (just the name before an existing `(`; `CURRENT_DATE`, `SYSDATE` without
parentheses).

- Built-ins for PostgreSQL, Oracle and SQLite (curated: syntax forms like `COALESCE`,
  `TRIM(… FROM …)`, `LISTAGG … WITHIN GROUP`, analytic/window functions).
- PostgreSQL: every function of the server (`pg_proc`, ~1000 names with all overloads and
  their descriptions, extensions and your own schemas included; other schemas as `schema.fn`),
  loaded in the background after connecting.
- Oracle: functions, procedures and package members of the allowed schemas; `pkg.` lists the
  package's members (also inside PL/SQL blocks), with argument names, types, `OUT` and
  defaulted (`[…]`) parameters.

Signature help inside a call shows the overloads with the argument being typed highlighted
(the first overload that takes that many arguments is selected).

#### Join completion

If the tables are connected with a foreign key sqlh can complete `JOIN` statements

![join_completion](imgs/sqls-fk_joins.gif)

Views join too: a view column taken from a table column inherits its foreign keys
(`v_orders.customer_id` → `customer.id`, "view column from orders"), and a view exposing a
table's primary key joins back to it. Relationships guessed from column names (`customer_id` →
`customer.id` without a declared foreign key) are offered last, labelled "maybe"
(`relations.maybe: false` to hide them).

#### Relationships

Hover and table completion show a table's or view's relationships: foreign keys both ways,
those exposed through views, guesses, the tables a view reads and the views reading a table.
From the CLI, `sqlh describe TABLE` / `sqlh relations TABLE`, and the table picker
(`sqlh tables -p --file F --line L --col C`) lists the tables related to the statement at the
cursor first and, after `JOIN`, inserts the whole join clause; Ctrl-R drills into a table's
relationships and inserts the join chain. See [doc/cli.md](doc/cli.md#relationships).

#### Running queries

The statement at the cursor, or every statement in a selection, runs on the current connection
and the result goes to a CSV file. Results are written to `{workspace}/.sqlh/results/`
(`results.dir`) as `{conn}_{stamp}.csv`, plus a `latest.csv` link. The `.sqlh/` directory
gitignores itself.

- **Run**: the first `results.previewLimit` rows (default 100). PostgreSQL reads through a
  server-side cursor, so only N rows are fetched. **Export**: all rows, streamed.
- Queries run in the background. Progress, the summary and errors appear in the editor's status
  line, not in popups. `sqlh cancel` (Helix `space i X`) cancels the running query.
- Statements are split by sqlh's own dialect-aware splitter (Oracle `;` vs PL/SQL `END;`,
  `$$` bodies, …).
- Results open in `results.viewer` (e.g. VisiData in a Herdr pane) with `--open` / `sqlh open`.
- After a run that executed DDL, the schema cache reloads.
- Every run is recorded in `{workspace}/.sqlh/history.jsonl`. Runs of the same query are
  grouped as versions, and the panel shows them with diffs.

From Helix this is bound under `space i` (`r` run, `e` export, `d` dump, `X` cancel). From an
LSP client, use the code actions. From the terminal, use `sqlh run` / `sqlh export`. See
[doc/cli.md](doc/cli.md#what-run--export-execute).

#### CLI

`sqlh <command>` uses the server of the current workspace (found by walking up from the
current directory or `--file`) over a Unix socket. It gets the same connection, the same
session and the editor's unsaved buffers. Without a server, `run`, `export`, `dump`,
`connections` and `latest` work standalone from the config.

```sh
sqlh run --sql "select * from orders" --limit 500   # → .sqlh/results/<conn>_….csv
sqlh export -f report.sql -l 12 --open              # the statement at line 12, all rows
sqlh dump reports/*.sql -d ~/exports/$(date +%F)    # each file on its header's connection
sqlh connect pg_local                               # switch the editor's connection
sqlh tables -p                                      # pick a table (inline picker)
sqlh describe orders                                # columns, keys, relationships
sqlh keys panel                                     # every panel key binding
```

The full command list, the pickers and the Helix keybindings are in [doc/cli.md](doc/cli.md).

#### Side panel

`sqlh panel` (Helix `space i t`, in [Herdr](https://herdr.dev)) opens a panel next to the
editor with five tabs (`1`–`5`): **Connections** (grouped; new, edit, duplicate, delete — a
form that writes the config file), **Objects** of the current connection, the **Statement** at
the cursor (its tables and their relationships), **Queries** (query files) and **History**
(runs and versions), with a preview fitting the selection (Describe/DDL/Sample, a query file,
a run and its diff; SQL highlighted). Enter navigates; Ctrl-Y inserts at
the cursor — after `JOIN` with its alias and `ON` condition, or as a whole join chain. `/`
searches like an omni bar (`:` tables, `.` columns, `@` connections, …). Every key is
configurable, per tab; `?` lists the tab's keys, searchable. `sqlh` (no command) is the
same as an app; the language server is `sqlh lsp`. The panel closes with its Helix. See
[doc/cli.md](doc/cli.md#panel).

#### Query files

A `-- sqlh: conn=ALIAS` header at the top of a file records the connection it last ran on in
the editor (the server keeps it up to date in the buffer as you run statements and switch
connections). `sqlh dump report.sql [-d DIR]` runs the file there from the terminal and
writes `report.csv`, e.g. from a scheduled job; Helix `space i d` dumps the statement at the
cursor, copying a recent complete result of the same query instead of running it again. A
header naming another connection than the editor's is flagged, with code actions to switch the
editor to it or to point the header at the editor's connection. See
[doc/cli.md](doc/cli.md#query-files-connection-header-and-dump).

#### Current schema

A connection works in one schema at a time: the first of its `schemas:` (or `schema:`), set on
every session (Oracle `ALTER SESSION SET CURRENT_SCHEMA`, PostgreSQL `SET search_path`), so a
read-only login works in the owner's schema with unqualified names, and completion,
relationships and go to definition follow. A connection is one database and schema: for
another, add a connection (the panel duplicates and edits them). See
[doc/cli.md](doc/cli.md#current-schema).

#### CodeAction

- [x] Run → CSV (first N rows), Export → CSV (all rows), Show results (text)
- [x] Cancel the running query
- [x] Switch the editor to the file header's connection / point the header at the editor's
- [x] Show / Switch Connections, Show Databases, Show Schemas, Show Tables
- [ ] Explain SQL

Code actions run on the selection, or on the statement at the cursor.

#### Hover

![hover](./imgs/sqls_hover.gif)

Columns of CTEs and derived tables show their type and the table column they come from.

#### Go to definition

Table aliases and WITH query names jump to their definition. A table or view — its name
(`t`, `schema.t`, also in DDL such as `ALTER TABLE t`), or one of its columns (`d.batch_name`,
`t.col`, or an unqualified column only one table of the statement has) — opens its definition
from the database: a read-only file
`<runtime dir>/views/<workspace key>/<connection>/<schema>.<name>.sql`, rewritten on every
request, at the line defining the column.

- Views: `CREATE OR REPLACE VIEW … AS …`, from the schema cache for the current schema, else
  fetched.
- Tables: Oracle `DBMS_METADATA.GET_DDL` when the login may use it (the owner, or
  `SELECT_CATALOG_ROLE`), otherwise — like DBeaver — generated from the `ALL_*` dictionary
  views (columns, constraints with their CHECK conditions, indexes incl. expressions,
  comments); PostgreSQL assembled from `pg_catalog`; SQLite `sqlite_master`. No triggers,
  grants or storage clauses. The first line says where it came from.
- The database is asked without blocking the CLI, for up to 5 s; if it doesn't answer the
  table is generated from the schema cache (marked approximate).
- Schemas: the current one and the others of the connection's `schemas:`; Oracle synonyms
  resolve to their table when it is in one of them.

Hover, go to definition and the CLI pickers (`sqlh tables -p --file …`: the directory maps back
to the workspace's server) work inside the file, so views on views can be followed down. Hover
marks views that can't be written (`view (read-only)`, `no INSERT`). `sqlh ddl TABLE` prints the
same definition.

#### Signature Help

![signature_help](./imgs/sqls_signature_help.gif)

#### Document Formatting

![document_format](./imgs/sqls_document_format.gif)

The formatter is safe for format-on-save: it handles JSON operators (`->`, `->>`), comments and
PL/SQL, and keyword case follows `lowercaseKeywords` (identifiers: `lowercaseIdentifiers`).

## Installation

```shell
go install github.com/raikhan/sqlh@latest
```

or from a checkout with [just](https://github.com/casey/just):

```shell
just install   # go install into $GOBIN / $GOPATH/bin, warns if another sqlh is first on PATH
just test
```

Editors start the language server as `sqlh lsp`.

## Editor Setup

### Helix

`~/.config/helix/languages.toml`:

```toml
[language-server.sqlh]
command = "sqlh"
args = ["lsp"]

[[language]]
name = "sql"
language-servers = ["sqlh"]
auto-format = true
```

The `space i` keybindings (run, export, dump, panel, table picker, connection switcher,
cancel, refresh) are listed in [doc/cli.md](doc/cli.md#helix). Helix discards the results of
`workspace/executeCommand`, so these bindings call the CLI in editor mode, and the server
reports back through the status line.

### Other editors

Any LSP client can run `sqlh lsp`. The upstream sqls plugins below target `sqls`; point
their command at `sqlh lsp` (their sqls-specific extras may not work):

- [sqls.vim](https://github.com/sqls-server/sqls.vim)
- [vscode-sqls](https://github.com/lighttiger2505/vscode-sqls)
- [sqls.nvim](https://github.com/nanotee/sqls.nvim)
- [Emacs LSP mode](https://emacs-lsp.github.io/lsp-mode/page/lsp-sqls/)

## DB Configuration

The connection to the RDBMS is essential to take advantage of the functionality provided by `sqlh`.
You need to set the connection to the RDBMS.

### Configuration Methods

There are the following methods for RDBMS connection settings, and they are prioritized in order from the top.
Whichever method you choose, the settings you make will remain the same.

1. Configuration file specified by the `-config` flag
1. `workspace/configuration` set to LSP client
1. Configuration file located in the following location
   - `$XDG_CONFIG_HOME`/sqlh/config.yml ("`$HOME`/.config" is used instead of `$XDG_CONFIG_HOME` if it's not set)

### Configuration file sample

An annotated reference with every option is in [doc/config.example.yml](doc/config.example.yml).

```yaml
# Set to true to use lowercase keywords instead of uppercase.
lowercaseKeywords: false
# Insert all-upper-case identifiers (Oracle's stored form) in lower case.
lowercaseIdentifiers: false
# Underline unknown tables, aliases and columns, checked against the schema cache
# (never the database). Default: true.
diagnostics: true
# Reload the schema cache after a run that executed DDL (CREATE, ALTER, DROP, RENAME,
# COMMENT). Default: true.
refreshAfterDDL: true
relations:
  # After JOIN, the table picker inserts "table alias ON …" (join) or the name (name).
  insert: join
  # Offer relationships guessed from column names ("maybe"). Default: true.
  maybe: true
results:
  dir: "{workspace}/.sqlh/results" # also {home}, {conn}, "~"
  previewLimit: 100 # rows for a preview run
  null: "" # text written for SQL NULL
  reuseFor: 10m # `space i d` reuses a complete result this recent
# viewer: ["vd"]                    # opens results; default open / xdg-open
panel:
  placement: right # the side panel: right, left, up or down
tui:
  queriesDir: queries # query files of the panel's Queries tab
connections:
  - alias: pg_local
    driver: postgresql
    dataSourceName: host=localhost port=5432 user=admin password=${PG_PASSWORD} dbname=app sslmode=disable
    schemas: [
      app,
      public,
    ] # metadata allow-list; the first is the current schema
    environment: dev # dev, test or prod
  - alias: oracle_dev
    driver: oracle # pure Go: no Oracle Instant Client
    host: oracle.example.com
    port: 1521
    dbName: ORCLPDB1 # service name
    user: app
    passwdCmd: security find-generic-password -s sqlh-oracle_dev -w
    schemas: [APP, APP_HIST]
    group: Oracle
  - alias: dsn_mysql
    driver: mysql
    dataSourceName: root:root@tcp(127.0.0.1:13306)/world
  - alias: individual_mysql
    driver: mysql
    proto: tcp
    user: root
    passwd: root
    host: 127.0.0.1
    port: 13306
    dbName: world
    params:
      autocommit: "true"
      tls: skip-verify
  - alias: mysql_via_ssh
    driver: mysql
    proto: tcp
    user: admin
    passwd: Q+ACgv12ABx/
    host: 192.168.121.163
    port: 3306
    dbName: world
    sshConfig:
      host: 192.168.121.168
      port: 22
      user: sshuser
      passPhrase: ssspass
      privateKey: /home/sqlh/.ssh/id_rsa
  - alias: dsn_vertica
    driver: vertica
    dataSourceName: vertica://user:pass@host:5433/dbname
```

### Workspace configuration Sample

- setting example with vim-lsp.

```vim
if executable('sqlh')
    augroup LspSqls
        autocmd!
        autocmd User lsp_setup call lsp#register_server({
        \   'name': 'sqlh',
        \   'cmd': {server_info->['sqlh', 'lsp']},
        \   'whitelist': ['sql'],
        \   'workspace_config': {
        \     'sqlh': {
        \       'connections': [
        \         {
        \           'driver': 'mysql',
        \           'dataSourceName': 'root:root@tcp(127.0.0.1:13306)/world',
        \         },
        \         {
        \           'driver': 'postgresql',
        \           'dataSourceName': 'host=127.0.0.1 port=15432 user=postgres password=mysecretpassword1234 dbname=dvdrental sslmode=disable',
        \         },
        \       ],
        \     },
        \   },
        \ })
    augroup END
endif
```

- setting example with coc.nvim.

In `coc-settings.json` opened by `:CocConfig`

```json
{
  "languageserver": {
    "sql": {
      "command": "sqlh",
      "args": ["lsp", "-config", "$HOME/.config/sqlh/config.yml"],
      "filetypes": ["sql"],
      "shell": true
    }
  }
}
```

- setting example with [nvim-lspconfig](https://github.com/neovim/nvim-lspconfig/blob/master/doc/server_configurations.md#sqls) (reusing its `sqls` server definition).

```lua
require'lspconfig'.sqls.setup{
  cmd = { "sqlh", "lsp" },
  on_attach = function(client, bufnr)
    require('sqls').on_attach(client, bufnr) -- require sqls.nvim
  end,
  settings = {
    sqlh = { -- sqlh reads its settings under "sqlh"
      connections = {
        {
          driver = 'mysql',
          dataSourceName = 'root:root@tcp(127.0.0.1:13306)/world',
        },
        {
          driver = 'postgresql',
          dataSourceName = 'host=127.0.0.1 port=15432 user=postgres password=mysecretpassword1234 dbname=dvdrental sslmode=disable',
        },
      },
    },
  },
}
```

- Setting example for Sublime Text 4

  Install the LSP Client by Opening the command palette and run `Package Control: Install Package`, then select `LSP`.

  Open `Preferences > Package Settings > LSP > Settings` and add the `"sqlh"` client configuration to the `"clients"`:

```
{
    "show_diagnostics_count_in_view_status": true,
    "clients": {
        "sqlh": {
            "enabled": true,
            "command": ["/path/to/sqlh", "lsp"],
            "selector": "source.sql"
        }
    }
}
```

**I'm sorry. Please wait a little longer for other editor settings.**

### Configuration Parameters

The first setting in `connections` is the default connection.

| Key                  | Description                                                                 |
| -------------------- | --------------------------------------------------------------------------- |
| connections          | Database connections                                                        |
| lowercaseKeywords    | Lowercase keywords in completion and formatting. Default `false`.           |
| lowercaseIdentifiers | Insert all-upper-case identifiers in lower case. Default `false`.           |
| diagnostics          | Schema-aware diagnostics. Default `true`.                                   |
| refreshAfterDDL      | Reload the schema cache after a run that executed DDL. Default `true`.      |
| relations            | `insert` (`join`/`name`), `maybe`, `chainDepth`, `chainLimit`               |
| results              | `dir`, `previewLimit`, `null`, `reuseFor`, `viewer`                         |
| panel                | `placement`, `insertKeys`, `keys`, `icons`, `statement`, `chains`, `syntax` |
| picker               | `keys` of the inline picker                                                 |
| tui                  | `queriesDir`, `editor`                                                      |

`sqlh keys` lists every configurable key binding. See [doc/cli.md](doc/cli.md#panel).

### connections

`dataSourceName` takes precedence over the value set in `proto`, `user`, `passwd`, `host`, `port`, `dbName`, `params`.

| Key            | Description                                                                                        |
| -------------- | -------------------------------------------------------------------------------------------------- |
| alias          | Connection alias name. Optional.                                                                   |
| driver         | `mysql`, `postgresql`, `oracle`, `sqlite3`, `mssql`, `h2`, `vertica`, `clickhouse`. Required.      |
| dataSourceName | Data source name (Oracle: a go-ora URL).                                                           |
| proto          | `tcp`, `udp`, `unix`. Default `tcp` when `host`/`port` is set.                                     |
| user           | User name                                                                                          |
| passwd         | Password                                                                                           |
| passwdCmd      | Shell command printing the password (e.g. macOS keychain). Used when `passwd` is empty.            |
| host           | Host                                                                                               |
| port           | Port                                                                                               |
| path           | unix socket path                                                                                   |
| dbName         | Database name (Oracle: service name)                                                               |
| params         | Option params. Optional.                                                                           |
| sshConfig      | ssh config. Optional.                                                                              |
| schemas        | Metadata allow-list (Oracle, PostgreSQL). The first is the current schema on connect. Optional.    |
| schema         | The schema the connection works in, instead of the first of `schemas`. Optional.                   |
| environment    | `dev` (default), `test` or `prod`: shown in the panel and `sqlh status`. Optional.                 |
| sample         | The panel's sample rows: `auto`, `manual` or `off`. Default: `auto` on dev/test, `manual` on prod. |
| group          | A folder in the panel's connection list (one level). Optional.                                     |

`${NAME}` in `user`, `passwd` and `dataSourceName` is expanded from the environment (a bare
`$NAME` is left literal).

#### sshConfig

| Key        | Description                 |
| ---------- | --------------------------- |
| host       | ssh host. Required.         |
| port       | ssh port. Required.         |
| user       | ssh user. Optional.         |
| privateKey | private key path. Required. |
| passPhrase | passPhrase. Optional.       |

#### DSN (Data Source Name)

See also.

- <https://github.com/go-sql-driver/mysql#dsn-data-source-name>
- <https://pkg.go.dev/github.com/jackc/pgx/v4>
- <https://github.com/mattn/go-sqlite3#connection-string>

## Contributors

This project exists thanks to all the people who contribute.
Most of the foundation comes from the [sqls contributors](https://github.com/sqls-server/sqls/graphs/contributors):
<a href="https://github.com/sqls-server/sqls/graphs/contributors">
<img src="https://contrib.rocks/image?repo=sqls-server/sqls" />
</a>

## Credits

sqlh is a fork of [sqls](https://github.com/sqls-server/sqls) by Toshikazu Ohashi
([lighttiger2505](https://github.com/lighttiger2505)), MIT licensed. sqls was inspired by the following OSS:

- [dbcli Tools](https://github.com/dbcli)
  - [mycli](https://www.mycli.net/)
  - [pgcli](https://www.pgcli.com/)
  - [litecli](https://litecli.com/)
- non-validating SQL parser
  - [sqlparse](https://github.com/andialbrecht/sqlparse)
