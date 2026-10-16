# sqlh: SQL Language Server

![test](https://github.com/raikhan/sqlh/workflows/test/badge.svg)

An implementation of the Language Server Protocol for SQL.

## Note

This project is currently under development and there is no stable release. Therefore, destructive interface changes and configuration changes are expected.

## Features

sqlh aims to provide advanced intelligence for you to edit sql in your own editor.

### Support RDBMS

- MySQL([Go-MySQL-Driver](https://github.com/go-sql-driver/mysql))
- PostgreSQL([pgx](https://github.com/jackc/pgx))
- SQLite3([go-sqlite3](https://github.com/mattn/go-sqlite3))
- MSSQL([go-mssqldb](https://github.com/microsoft/go-mssqldb))
- H2([pgx](https://github.com/CodinGame/h2go))
- Vertica([vertica-sql-go](https://github.com/vertica/vertica-sql-go))

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

![completion](./imgs/sqlh-completion.gif)

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
If the tables are connected with a foreign key sqlh can complete ```JOIN``` statements

![join_completion](imgs/sqlh-fk_joins.gif)

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

![code_actions](https://github.com/raikhan/sqlh.vim/blob/master/imgs/sqls_vim_demo.gif)

- [x] Execute SQL
- [ ] Explain SQL
- [x] Switch Connection(Selected Database Connection)
- [x] Switch Database

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

## Installation

```shell
go install github.com/raikhan/sqlh@latest
```

## Editor Plugins

- [sqlh.vim](https://github.com/raikhan/sqlh.vim)
- [vscode-sqlh](https://github.com/lighttiger2505/vscode-sqlh)
- [sqlh.nvim](https://github.com/nanotee/sqlh.nvim)
- [Emacs LSP mode](https://emacs-lsp.github.io/lsp-mode/page/lsp-sqlh/)

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

```yaml
# Set to true to use lowercase keywords instead of uppercase.
lowercaseKeywords: false
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
connections:
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

- setting example with [nvim-lspconfig](https://github.com/neovim/nvim-lspconfig/blob/master/doc/server_configurations.md#sqlh).

```lua
require'lspconfig'.sqlh.setup{
  cmd = { "sqlh", "lsp" },
  on_attach = function(client, bufnr)
    require('sqlh').on_attach(client, bufnr) -- require sqlh.nvim
  end
  settings = {
    sqlh = {
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

  Install the LSP Client by Opening the command palette and run ```Package Control: Install Package```, then select ```LSP```.

  Open ```Preferences > Package Settings > LSP > Settings``` and add the ```"sqlh"``` client configuration to the ```"clients"```:
```
{
    "show_diagnostics_count_in_view_status": true,
    "clients": {
        "sqlh": {
            "enabled": true,
            "command": ["/path/to/sqlh binary"],
            "selector": "source.sql"
        }
    }
}
```

**I'm sorry. Please wait a little longer for other editor settings.**

### Configuration Parameters

The first setting in `connections` is the default connection.

| Key         | Description          |
| ----------- | -------------------- |
| connections | Database connections |

### connections

`dataSourceName` takes precedence over the value set in `proto`, `user`, `passwd`, `host`, `port`, `dbName`, `params`.

| Key            | Description                                 |
| -------------- | ------------------------------------------- |
| alias          | Connection alias name. Optional.            |
| driver         | `mysql`, `postgresql`, `sqlite3`, `mssql`, `h2`. Required. |
| dataSourceName | Data source name.                           |
| proto          | `tcp`, `udp`, `unix`. Default `tcp` when `host`/`port` is set. |
| user           | User name                                   |
| passwd         | Password                                    |
| host           | Host                                        |
| port           | Port                                        |
| path           | unix socket path                            |
| dbName         | Database name                               |
| params         | Option params. Optional.                    |
| sshConfig      | ssh config. Optional.                       |
| schemas        | Metadata allow-list (Oracle, PostgreSQL). The first is the current schema on connect. Optional. |
| schema         | The schema the connection works in, instead of the first of `schemas`. Optional. |
| group          | A folder in the panel's connection list (one level). Optional. |

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
<a href="https://github.com/raikhan/sqlh/graphs/contributors">
    <img src="https://contrib.rocks/image?repo=sqlh/sqlh" />
</a>

## Inspired

I created sqlh inspired by the following OSS.

- [dbcli Tools](https://github.com/dbcli)
    - [mycli](https://www.mycli.net/)
    - [pgcli](https://www.pgcli.com/)
    - [litecli](https://litecli.com/)
- non-validating SQL parser
    - [sqlparse](https://github.com/andialbrecht/sqlparse)
