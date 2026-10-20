<p align="center">
  <img src="docs/icon.svg" alt="" width="112" height="112">
</p>

<h1 align="center">Rowsmith<sup>™</sup></h1>

<p align="center">
  <a href="https://github.com/abdulwaheedsyed/Rowsmith/releases"><img src="https://img.shields.io/github/v/release/abdulwaheedsyed/Rowsmith?include_prereleases&sort=semver" alt="Latest release"></a>
  <a href="https://github.com/abdulwaheedsyed/Rowsmith/actions/workflows/ci.yml"><img src="https://github.com/abdulwaheedsyed/Rowsmith/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/abdulwaheedsyed/Rowsmith/actions/workflows/codeql.yml"><img src="https://github.com/abdulwaheedsyed/Rowsmith/actions/workflows/codeql.yml/badge.svg" alt="CodeQL"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License: Apache 2.0"></a>
</p>

**A fast, self-hosted web workspace for your databases.** Browse, query, edit and administer MySQL, MariaDB, TiDB, PostgreSQL + PostGIS, CockroachDB, YugabyteDB, SQL Server, Oracle, ClickHouse, MongoDB, Redis, Valkey, Cassandra, ScyllaDB, Elasticsearch, OpenSearch, BigQuery, Snowflake, Databricks, Trino (with Iceberg and Delta Lake tables and Parquet, ORC or CSV files) and SQLite from one place, with built-in SSH tunnels, a team vault for connection secrets, and a UI designed for both desktop and phone.

Rowsmith is a single Go binary with the web app embedded. It is designed to run in its own container behind your existing reverse proxy.

> **Status: early development.** All twenty-one engines, the web UI, SSH tunnels, the team vault, the structure editor, import and export, the AI assistant, scheduled queries, shared query links with comments, and database migration are working. MySQL, MariaDB, TiDB, PostgreSQL/PostGIS, CockroachDB, YugabyteDB, ClickHouse, MongoDB, Redis, Valkey, Cassandra, ScyllaDB, Elasticsearch, OpenSearch and Trino are covered by an end-to-end test; SQL Server, Oracle, SQLite, BigQuery, Snowflake and Databricks have unit tests and integration tests you can run against real servers (Snowflake's and Databricks' against an account of your own). See [Roadmap](#roadmap).

---

## Why Rowsmith

| Strength | What you get |
|---|---|
| **One workspace, many engines** | A common driver interface covers browsing, filtering, editing, SQL execution, EXPLAIN plans, process lists, variables and users. Each engine adds only what it supports, and new engines plug into the same registry. |
| **Built for teams** | Owner, admin, member and viewer roles. Share a connection with the whole team or with specific people, with read, write or manage access. Viewers are always read-only. |
| **Secrets handled properly** | Connection passwords, SSH keys, TLS keys and API keys are envelope-encrypted before they touch disk, and they are never sent back to the browser. |
| **Private servers, no extra software** | SSH tunnels, including chains of jump hosts, run inside the server process. New host keys must be approved once, and changed keys are rejected. |
| **Safety rails for production** | Statements are classified before they run. On production connections, anything that may modify data needs confirmation. `DROP`, `TRUNCATE`, and `DELETE` or `UPDATE` without `WHERE` always need it. Read-only access is enforced by the database session and by Rowsmith. |
| **An audit trail** | Sign-ins, connection changes, sharing, data edits and every data-modifying query are recorded with who, when and from where. |
| **Spatial data on a map** | PostGIS, MySQL, MariaDB, SQL Server and Oracle Spatial geometry is decoded to GeoJSON, ready to show on a map. |
| **Move between servers and engines** | Migrate tables and rows from one server to another, or from MySQL to PostgreSQL, SQL Server to MySQL, MongoDB to SQL and back. Every column's type is mapped for you to review, and the copy is checked value by value. |
| **Discuss queries where they live** | Send a teammate a link to a query, optionally with its result. Comment on any line, @mention people, reply and resolve, and get notified in the app or by email. |
| **Reports and alerts that run themselves** | Schedule any read-only query to email its result as Excel, CSV or JSON, post to Slack, Teams or any webhook, or alert only when a row count, a value or the whole result changes. |
| **An assistant on your own terms** | Ask for queries in plain words, or have one explained, fixed or made faster. Bring your own Anthropic, OpenAI or OpenRouter key, or point it at a model you host with Ollama. It reads the schema, never changes data, and only sees rows if you allow it. |

## Features

### Working today

- **Data browser**: server-side filters (`=`, `<`, `LIKE`, contains, `IN`, `NULL`, regex, `BETWEEN`), quick search across columns, sorting, paging, and exact or estimated row counts
- **Safe row editing**: batched inserts, updates and deletes in a single transaction. Each change must match exactly one row or the whole batch rolls back. Tables without keys are handled through `ctid` (PostgreSQL) or an all-column match with `LIMIT 1`.
- **SQL console** with streamed results:
  - dialect-aware script splitting: MySQL `DELIMITER`, T-SQL `GO`, Oracle `/`, dollar quoting, `BEGIN…END` bodies
  - multiple result sets and server notices
  - transactions that stay open across runs
  - cancelling a running query
- **Structure**: columns, indexes, foreign keys in both directions, checks, triggers, partitions and generated DDL
- **Structure editor**: create and change tables (or MongoDB collections) from a form, with the exact SQL for your engine shown live as you edit. Columns can be renamed, reordered where the engine allows it, typed, keyed and computed; indexes, foreign keys, checks and table options are edited in place. Each engine only offers what it can do, and dropping columns asks first. Also: creating and dropping databases and schemas, renaming, emptying and dropping tables and other objects (from a table's ⋯ menu, its tabs, or the connection menu). Every drop previews its SQL and asks you to type the name. Rowsmith closes its own idle sessions on a database before dropping it; "Disconnect everyone using it first" ends other sessions on PostgreSQL 13+ and SQL Server.
- **Export**: tables (with the grid's filters and sort), query results and whole schemas to CSV, TSV, JSON, NDJSON, Excel or SQL INSERT statements, optionally gzipped. Files are prepared on the server with live progress, so exports are not limited to the rows on screen.
- **SQL dumps**: a script that recreates tables, rows, keys, views, routines and triggers, restorable with the database's own client (`mysql`, `psql`) or with Rowsmith. Handles partitions, sequences and identity columns, and leaves out objects that extensions such as PostGIS create themselves.
- **Import**: CSV (delimiter detected), TSV, JSON, NDJSON and Excel into an existing table, with a preview and column mapping, in one transaction: if a row fails, nothing is kept. SQL files run statement by statement with the console's safety rules.
- **EXPLAIN / EXPLAIN ANALYZE** turned into a plan tree. PostgreSQL's `ANALYZE` runs inside a transaction that is always rolled back.
- **Administration**: running processes (with kill), server variables and status
- **Sizes**: the connection's overview lists its databases and the tables in the one you're in, largest first, with size on disk (indexes included) and rows. The navigator can show each table's size instead of its row count.
- **Users and access**: manage database accounts and what they may do without writing GRANT statements. See [Users and access](#users-and-access).
  - a privilege editor per user or role: tick privileges server-wide, on a database or schema, on every table (including tables created later, where the engine supports it), or table by table in a matrix
  - quick presets (read only, read & write, full access, no access) and copying another account's access
  - access a user gets through roles, PUBLIC or a broader grant shown as such, with a link to where it's changed
  - creating, locking and dropping users and roles, changing passwords, account settings and role memberships
  - an Access section on each table's Structure tab: who can do what on it
  - every change previewed as SQL before it runs
- **Type fidelity**: exact decimals, 64-bit integers, binary previews with image detection, and dates exactly as the server stores them
- **Accounts**:
  - Argon2id password hashing
  - two-step sign-in (TOTP) with recovery codes
  - account lockout and rate limiting
  - session management
  - first-run setup protected by a one-time code
- **Query history and saved queries** (private or shared with the team), plus notes on connections and objects
- **Migration**: copy a database or schema to another connection, on the same engine or a different one, with every column type mapped and reviewable, rows streamed in chunks, keys and indexes added afterwards, and a value-by-value check of the result. See [Migration](#migration).
- **Shared query links**: share a query with the team or chosen people, optionally with a snapshot of its result and an expiry date. Teammates comment on the whole query or on a single line, reply, resolve threads and @mention each other. A bell collects shares, mentions and replies. See [Sharing and comments](#sharing-and-comments).
- **Scheduled queries**: run a query every few minutes, hourly, on chosen weekdays or monthly (or with a cron expression), in any time zone. Send the result by email (attached, with a preview in the message) or to a webhook, or alert only when a condition is met. Each run is kept with its file for download. See [Schedules](#schedules).
- **AI assistant** in every query tab (`Ctrl+I`): write queries from a description, explain them, fix a failed statement from its error, or speed one up from its plan. Answers stream with SQL you can insert, replace or run. See [AI assistant](#ai-assistant).

### Engines

| Engine | Status |
|---|---|
| MySQL 5.7 / 8.x / 9.x (incl. Percona, Aurora MySQL) | ✅ Working |
| MariaDB 10.x / 11.x | ✅ Working |
| TiDB 7.5+ and TiDB Cloud | ✅ Working |
| PostgreSQL 10+ with PostGIS, TimescaleDB, Supabase, Neon, Aurora, AlloyDB, Cloud SQL | ✅ Working |
| CockroachDB 24.1+ and CockroachDB Cloud | ✅ Working |
| YugabyteDB 2.20+ (YSQL) and YugabyteDB Aeon | ✅ Working |
| Microsoft SQL Server 2012+ / Azure SQL | ✅ Working |
| Oracle Database 12c+ | ✅ Working |
| ClickHouse 24.8+ and ClickHouse Cloud, over the native protocol or HTTP | ✅ Working |
| SQLite 3 | ✅ Working |
| MongoDB 5+ | ✅ Working |
| Redis 6+ (incl. Redis Stack, Redis Cluster and ElastiCache) | ✅ Working |
| Valkey 7.2+ | ✅ Working |
| Apache Cassandra 4.0+ and DataStax Enterprise | ✅ Working |
| ScyllaDB 6+ and ScyllaDB Cloud | ✅ Working |
| Elasticsearch 8 and 9, and Elastic Cloud | ✅ Working |
| OpenSearch 2 and 3, and Amazon OpenSearch Service | ✅ Working |
| Google BigQuery | ✅ Working |
| Snowflake (AWS, Azure, Google Cloud) | ✅ Working |
| Databricks SQL warehouses with Unity Catalog (AWS, Azure, Google Cloud) | ✅ Working |
| Trino 4xx, with Iceberg and Delta Lake tables and Hive tables over Parquet, ORC or CSV files | ✅ Working |

Every driver is pure Go, so no Oracle Instant Client or Microsoft ODBC install is needed.

**Engine notes**

- **CockroachDB, YugabyteDB and TiDB** use the PostgreSQL and MySQL drivers, which ask each server for what it keeps differently. Each has its own entry in the connection form with its default port (26257, 5433, 4000), and `postgres://` or `mysql://` URLs fill it too.
  - **CockroachDB**: TLS is required by default, as on CockroachDB Cloud. Row counts come from table statistics. Tables without a primary key are edited by their hidden `rowid`, which exports and migrations leave out. Plans show CockroachDB's own tree, with rows read and time per step when analyzed. Processes are the cluster's sessions, which can be cancelled; the status tab shows jobs; variables show cluster and session settings. All of this works without `crdb_internal`, which recent versions restrict.
  - **YugabyteDB**: everything PostgreSQL offers, with YugabyteDB's version and its LSM and GIN index types in the structure editor.
  - **TiDB**: plans in TiDB's form, with the rows each operator read and its time when analyzed. The structure editor applies one change per `ALTER TABLE`, as TiDB's online schema changes need. CHECK constraints show when the server enforces them (`tidb_enable_check_constraint`). The access editor reads grants from `mysql.db` and `mysql.tables_priv`, since TiDB leaves `information_schema`'s privilege views empty. Migrations into TiDB store spatial columns as GeoJSON.
- **SQL Server**: `GO` batches, `PRINT` and `RAISERROR` messages as notices, showplan XML turned into a plan tree, read-only application intent for replicas, and `geometry`/`geography` on the map.
- **Oracle**: connect by service name or SID, optional `SYSDBA`-style roles, PL/SQL blocks split on `/`, `DBMS_OUTPUT` shown as notices, and packages, synonyms and sequences in the navigator. `SDO_GEOMETRY` columns appear on the map. When Oracle Spatial is installed (it is included in every edition, but left out of some slim container images), the structure editor registers new geometry columns in `USER_SDO_GEOM_METADATA` with their SRID and can create spatial indexes.
- **ClickHouse**: the native protocol (port 9000, 9440 with TLS) or HTTP (8123, 8443); the form moves the port as you switch. The navigator shows tables, views, materialized views (with the table they feed) and dictionaries; the Structure tab shows the engine, ORDER BY, partitions, TTL, codecs, skip indexes and MATERIALIZED / ALIAS columns, and the structure editor creates and changes them. Arrays, maps (keys in order), tuples, JSON, big integers, IPs and geo types render as such, and `Point` and `Polygon` go on the map. The console keeps `SET` and `USE` between runs (over HTTP through a server session), stops a huge result at the row limit on the server instead of reading it to the end, and shows how many rows and bytes each statement read. EXPLAIN shows the plan with the granules each index skips, and Explain analyze runs a read-only query to measure it. There are no transactions, so grid edits are checked first and then run as mutations (`ALTER TABLE … UPDATE`, lightweight `DELETE`) matched on the row's values; imports load into a staging table and move in one `INSERT … SELECT`, so a bad row leaves the table untouched. Read-only connections also run with `readonly = 2` on the server.
- **SQLite**: databases are files on the Rowsmith server, so the driver only opens files inside `ROWSMITH_SQLITE_DIR`. `ATTACH` is disabled and Rowsmith's own metadata database is refused. Use `:memory:` for a scratch database.
- **MongoDB**: a console that understands mongosh syntax (`db.orders.find({...}).sort(...)`, `aggregate`, CRUD, index and collection commands) without running JavaScript on the server. Field lists are inferred from a sample of documents, and edits keep BSON types.
- **Redis and Valkey**: keys are listed by type in the navigator (strings, hashes, lists, sets, sorted sets, streams and JSON documents), up to 2,000 at a time; type a name or a pattern such as `user:*` and the server finds the matching keys with `SCAN`, so large databases stay responsive. Each numbered database (`db0`, `db1`…) shows its key count. Opening a key shows a grid for its type: a hash's fields, a list's items by position, a sorted set's members by score, a stream's entries with their fields. Every grid can be filtered, searched, sorted and edited; a save checks that each field, member, item or entry is still there, then applies the changes in one `MULTI … EXEC` that fails if the key changed meanwhile. New keys of any type and expiry times are set from dialogs that show the commands they run. The console reads redis-cli syntax, one command per line, and lays replies out as tables (`HGETALL` as fields and values, `ZRANGE … WITHSCORES` as members and scores, `INFO` by section); `SELECT` and `MULTI` carry between runs, and cancelling stops a blocking command. `FLUSHDB`, `FLUSHALL` and `SHUTDOWN` always ask for confirmation. Sign in with a password or an ACL user, over TLS and SSH tunnels; `redis://`, `rediss://` and `valkey://` URLs fill the form. Clients and `INFO`/`CONFIG` appear under processes and variables. **Redis Cluster** is recognised when you connect to any of its nodes: the navigator lists the keys of every primary, browsing and edits go to the node that owns each key, and processes show every node's clients. In the console, commands follow the cluster's redirects; `KEYS`, `DBSIZE` and `FLUSHDB` run on every primary, while `SELECT`, `SCAN` and `MULTI … EXEC` are refused with the reason, since a cluster has only database 0 and the console keeps no connection to one node. Commands on several keys need the keys in one slot, which a shared hash tag (`{cart:7}:items`, `{cart:7}:owner`) gives them.
- **Cassandra and ScyllaDB**: keyspaces are the databases, each with its replication, and the navigator lists tables, materialized views, user types, functions and aggregates. List one or more nodes to reach the cluster through, with an optional local data center; *Use only the listed hosts* keeps the driver to them, for SSH tunnels and networks where the addresses nodes announce can't be reached. The Structure tab shows the partition key, clustering order, static columns, indexes (SAI too), time to live, compaction and the table's CQL. Values keep their types: user types, maps, sets, lists, tuples and vectors as JSON, exact decimals and varints, timeuuids and durations. The grid filters with what Cassandra allows (=, ranges, IN, CONTAINS on collections, adding `ALLOW FILTERING` for other columns) and sorts by clustering columns once the whole partition key is filtered. Every table has a primary key, so each edit is a lightweight transaction on it: an insert never overwrites a row (`IF NOT EXISTS`) and changes go only to rows that still exist (`IF EXISTS`); counter tables and views can't be edited. Rows aren't counted from estimates, which count partitions, so a count reads the table (with a time limit when unfiltered). The console splits batches, keeps `USE`, `CONSISTENCY`, `SERIAL CONSISTENCY` and `TRACING` between runs, and explains that cqlsh's file commands (`COPY`, `SOURCE`) belong to Import and Export. Explain runs a SELECT with tracing, since CQL has no EXPLAIN, and shows each node's steps. Processes are the connected clients, variables the node's settings, and the status tab lists the cluster's nodes. On ScyllaDB the views behind secondary indexes stay out of the navigator. Migrations keep the partition key between Cassandra and ScyllaDB; from other engines the first primary key column becomes the partition key, and tables without a primary key are left out.
- **Elasticsearch and OpenSearch**: sign in with a user and password or an API key, over HTTPS or HTTP, through SSH tunnels too, with an optional path prefix for proxies. The navigator lists indices (with their document counts and size), data streams and aliases; hidden and system indices stay out. The Structure tab shows each field's mapping type, the index's shards, replicas and refresh interval, and the request that creates it. The grid searches with typed filters (term and range on keyword, numeric and date fields, phrase matches on text, wildcards, exists), the search box runs a simple query string over every field, and sorting uses a text field's keyword sub-field. Objects and arrays show as JSON and `geo_point` and `geo_shape` fields go on the map. Edits are checked by sequence number, so a document changed meanwhile isn't overwritten, inserts refuse an `_id` that's taken, and data streams take new documents only. The console speaks Kibana Dev Tools syntax (`GET /orders/_search` then the JSON body, `"""` strings, `_bulk` bodies line by line) and lays replies out as tables: hits, each aggregation's buckets and metrics, `_cat` APIs, SQL, PPL and ES|QL results. `DELETE` of an index and `_delete_by_query` ask for confirmation. Explain runs the search with profiling and shows each shard's query tree with timings; processes are running tasks, which can be cancelled; variables are the cluster settings with their defaults, and the status tab lists nodes with their load. The structure editor creates indices (a text field gets a keyword sub-field) and adds fields and settings; changing a field's type needs a reindex, which it says. A primary key of one field becomes each document's `_id`, so imports and migrations replace documents instead of duplicating them.
- **Snowflake**: sign in with a key pair (encrypted keys too), a programmatic access token, an OAuth token or a password, with an optional warehouse, role, database and schema; `snowflake://` URLs fill the form. The navigator, Structure tab, keys and autocomplete come from `SHOW` commands and `GET_DDL`, which run in Snowflake's cloud services without waking a warehouse; only browsing data and queries use it. `VARIANT`, `OBJECT` and `ARRAY` columns show as JSON and are edited as JSON, `GEOGRAPHY` and `GEOMETRY` go on the map, and numbers stay exact. Primary, unique and foreign keys are recorded but not enforced by Snowflake, so a save still checks that each change matches exactly one row. The console splits `$$` bodies and Snowflake Scripting blocks, keeps `USE`, `ALTER SESSION` and transactions between runs, and refuses `PUT` and `GET`, which would read and write files on the Rowsmith server. Plans come from `EXPLAIN USING JSON` with the partitions each scan reads. Processes are the queries running in the last hour (cancel with `SYSTEM$CANCEL_QUERY`), variables are session parameters, and the status tab shows warehouses. Migrations write upper-case names by default, as unquoted names are in Snowflake.
- **Trino**: a catalog is a database in Rowsmith, each shown with its connector, and the navigator lists schemas, tables, views and materialized views. Sign in with a user alone, a password or an access token (both need TLS), through SSH tunnels too; `trino://user@host:8080/catalog/schema` URLs fill the form (`trinos://` for TLS). **Iceberg** tables show their partitioning, sort order, format, location, snapshot count, last commit, and data and delete files, all read from metadata tables, so neither the Structure tab nor row counts scan data. **Delta Lake** tables show their partition columns, location, table properties, version count, last commit, files and size, read from the transaction log, with a row estimate from table statistics. Iceberg and Delta Lake tables have no keys, so grid edits match a row on all of its values and must find exactly one. Both take one statement per transaction, so a save checks every row first and then applies the changes one by one (each a commit of its own). **Hive** tables over Parquet, ORC, CSV or JSON files are browsed and queried like any other, and read-only in the grid. The structure editor creates tables with their format, partitioning (`month(placed_at)`, `bucket(id, 16)`) and location, and changes columns, comments and properties; there are no indexes or enforced keys. Arrays, maps and rows show as JSON with their field names, and filters compare values in the column's own type. The console keeps `USE`, `SET SESSION` and transactions between runs and reads Trino's time travel (`FOR VERSION AS OF`) and metadata tables (`"orders$snapshots"`, `"payments$history"`). Plans show the distributed plan as one tree, each fragment under the exchange that reads it; Explain analyze adds the rows and CPU time of every operator. Running queries can be stopped, and variables are the session properties. Migrations into Trino write the types the target catalog's format takes (Iceberg, Delta Lake or Hive), without keys, indexes or defaults, and JSON as text.
- **Databricks**: connect to a SQL warehouse (its workspace host and HTTP path, from the warehouse's Connection details) with a personal access token or a service principal (OAuth), through SSH tunnels too; `databricks://` URLs fill the form. Unity Catalog's catalogs are the databases, and the navigator lists tables, views, materialized views, streaming tables, volumes and functions. Delta tables show their format, location, partition or clustering columns, files, size, version and last change, informational primary keys and enforced CHECK constraints, and the table's own `SHOW CREATE TABLE`. Arrays, maps, structs and VARIANT values show as JSON with their fields in order, and decimals stay exact. Delta tables can be edited: each change is checked to match exactly one row before it runs, since warehouse statements commit one by one. The structure editor creates Delta tables (partitioned or liquid-clustered, with defaults, NOT NULL and CHECK constraints) and changes them; renaming or dropping a column turns on column mapping first, which the script shows. Explain shows the Spark physical plan; run-time figures stay in the workspace's query profile. Processes are the warehouse's running queries from the query history, variables the session's settings, and the status tab the warehouse's state and size. A stopped warehouse starts on the first query, which takes a few seconds on serverless.
- **BigQuery**: sign in with a service-account key or Application Default Credentials. Every query is dry-run first; queries estimated to process more than the connection's limit (10 GB by default) are refused, and each job carries the limit. Table previews use the free `tabledata.list` API. The service account needs `roles/bigquery.jobUser` on the project and `roles/bigquery.dataViewer` (or `dataEditor`) on the datasets.

## Roadmap

- [x] Backend core: vault, auth, store, driver interface, SSH tunnels, API
- [x] MySQL, MariaDB, TiDB, PostgreSQL/PostGIS, CockroachDB, YugabyteDB, SQL Server, Oracle, ClickHouse, SQLite, MongoDB, Redis, Valkey, Cassandra, ScyllaDB, Elasticsearch, OpenSearch, BigQuery, Snowflake, Databricks and Trino drivers
- [x] Web UI: connection manager, navigator, virtualized data grid, SQL editor with schema-aware autocomplete, plan viewer, map view, command palette, mobile layout
- [x] Relationship (ER) diagrams
- [x] Production container image and reverse-proxy examples
- [x] Structure editor for every engine
- [x] Import and export (SQL dump, CSV, TSV, JSON, NDJSON, Excel)
- [x] AI SQL assistant: write, explain, fix and speed up queries from your schema, with Anthropic, OpenAI, OpenRouter or a self-hosted model
- [x] Scheduled queries and exports with email and webhook delivery and threshold alerts
- [x] Shareable query links and comments
- [x] Database migration between servers and across engines (schema translation, data copy, verification)
- [x] Visual management of database users, roles and privileges
- [x] Releases: container images for amd64 and arm64, and the server for Linux, macOS and Windows, with checksums, a software bill of materials and signed build provenance

## Architecture

```
cmd/rowsmith          entry point and CLI (serve, create-user, reset-password, rotate-key, healthcheck)
internal/api          JSON + NDJSON streaming API, auth middleware, CSRF, security headers
internal/ai           SQL assistant: Anthropic and OpenAI-compatible providers, read-only schema tools
internal/schedule     scheduled queries: cron timetables, alerts, encrypted result files, email and webhook delivery
internal/migrate      migrations: type mapping across engines, plans, streamed copy, verification
internal/mail         SMTP client: STARTTLS/TLS, PLAIN/LOGIN sign-in, MIME with streamed attachments
internal/auth         Argon2id, TOTP, recovery codes, session tokens, rate limiting
internal/vault        envelope encryption for secrets at rest
internal/store        embedded SQLite metadata store with migrations
internal/session      live connection pools, read-only pools, pinned console sessions
internal/tunnel       SSH tunnels with jump hosts and host-key trust
internal/sqlsplit     dialect-aware SQL splitter and statement classifier
internal/driver       driver contract, value encoding, TLS helpers, registry
  sqlbase             shared engine for database/sql drivers (browse, edit, stream)
  mysql, postgres …   one package per engine
internal/web          embedded single-page app
web/                  React + TypeScript + Vite frontend
dev/                  throwaway test databases, SSH bastion, smoke test, release scripts
```

The frontend is compiled into the Go binary. Rowsmith keeps its own data (users, connections, history) in a SQLite file in the data directory. It needs no external database.

## Security model

To report a vulnerability, see the [security policy](SECURITY.md).

- **Encryption at rest.** Each secret is encrypted with its own random data key using XChaCha20-Poly1305. That data key is then wrapped with a key-encryption key derived from the master key by HKDF-SHA256. Every envelope is bound to its record and field, so a ciphertext copied into another row will not decrypt.
- **Master key.** Supply it as a Docker secret (`/run/secrets/rowsmith_master_key`), through `ROWSMITH_MASTER_KEY_FILE`, or through `ROWSMITH_MASTER_KEY`. If none is given, Rowsmith generates `<data>/keys/master.key` with `0600` permissions on first start. **Back the key up separately from the data directory.** Without it, saved secrets cannot be recovered. `rowsmith rotate-key` rotates it and re-wraps every stored secret.
- **Exports and uploads** are written to `<data>/spool`, each encrypted with its own random key that exists only in memory, readable only by the user who created them, and deleted after 15 minutes (or when an import finishes). Exports always read through read-only database sessions.
- **Secrets are write-only.** The API reports only whether a secret is set. Users with read or write access see only a connection's host and database, never its credentials.
- **Sessions** use random 256-bit tokens, stored hashed. Cookies are `HttpOnly`, `Secure` and `SameSite=Strict`, and use the `__Host-` prefix when served at the root path. Sessions have idle and absolute timeouts and are rotated after sign-in and MFA.
- **Request protection.** Every state-changing request needs a per-session CSRF token, a same-origin `Origin` header and a JSON body. The server sends a strict Content Security Policy with no inline scripts and `frame-ancestors 'none'`, plus HSTS.
- **First run.** No account exists until someone enters the one-time setup code printed in the server log. A freshly deployed instance cannot be claimed by a stranger.
- **Read-only enforcement** has two layers:
  1. Rowsmith opens read-only database sessions where the engine supports them (for example `SET SESSION TRANSACTION READ ONLY`, or `default_transaction_read_only`).
  2. Rowsmith blocks statements it cannot prove are reads, including attempts to switch the session back to read-write.

  For guarantees against a determined user, also give read-only people read-only database credentials.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ROWSMITH_ADDR` | `:8080` | Listen address |
| `ROWSMITH_DATA_DIR` | `/data` | Metadata database, generated keys, exports |
| `ROWSMITH_PUBLIC_URL` | | External URL, e.g. `https://db.example.com` or `https://example.com/sql`. Sets the base path and the allowed Origin. |
| `ROWSMITH_MASTER_KEY_FILE` | `/run/secrets/rowsmith_master_key` if present | File with one base64 key per line. The first line is the current key. |
| `ROWSMITH_MASTER_KEY` | | Base64 master key, as an alternative to the file |
| `ROWSMITH_TRUSTED_PROXIES` | private ranges | CIDRs allowed to set `X-Forwarded-For` |
| `ROWSMITH_SESSION_IDLE` | `8h` | Idle session timeout |
| `ROWSMITH_SESSION_MAX` | `72h` | Absolute session lifetime |
| `ROWSMITH_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `ROWSMITH_SQLITE_DIR` | `<data>/sqlite` | The only directory SQLite connections may open files from |
| `ROWSMITH_MAX_UPLOAD_MB` | `1024` | Largest file accepted for import |
| `ROWSMITH_INSECURE_COOKIES` | `false` | Local plain-HTTP development only |

## Schedules

Schedule a query from its query tab (the calendar button), from a saved query, or from the **Schedules** page. Choose when it runs, what to send, and who gets it. **Run the query** in the dialog shows what it returns and whether the alert would fire now, without sending anything.

- **When**: every 5 to 30 minutes, every 1 to 12 hours, on chosen days of the week, on a day of the month (or the last day), or a five-field cron expression. Times follow the schedule's time zone, including daylight saving changes. Schedules run at most every 5 minutes. A run missed while Rowsmith was down runs once when it starts again.
- **What**: the first result set, as Excel, CSV, TSV, JSON or NDJSON (optionally gzipped), up to 1,000,000 rows. Emails can attach the file (up to 10 MB) and show the first rows in the message.
- **Alerts**: instead of sending every run, send only when the row count or a value in the first row passes a threshold, or when the result changes from the previous run. You can choose to be told once when a condition starts, not on every run while it lasts.
- **Where**: email, and webhooks. Slack, Microsoft Teams, Google Chat and Discord get a message; other URLs get the result as JSON.
- **Safety**: a schedule runs with its owner's access, in a read-only session, and only accepts statements that read. It pauses itself if the owner loses access or is disabled, or after 5 failures in a row. Admins see everyone's schedules and can pause or delete them, but only the owner can change one. Viewers can receive results but not create schedules.
- **History**: every run is recorded with its row count, what was sent, and any error. Result files are encrypted at rest (XChaCha20-Poly1305 per 64 KiB chunk, key sealed with the master key) and deleted after 14 days by default.

An admin sets up email under **Administration → Email & schedules**: an SMTP server (STARTTLS, TLS, or a trusted relay without encryption), with presets for common providers and a test message. The same page sets who may receive results: team members only, team members plus listed domains, or anyone. It also controls whether webhooks are allowed, and whether they may reach private network addresses (blocked by default, including names that resolve to them).

## Migration

**Migrate** (in the rail, the command palette, or a connection's menu) copies tables from one connection to another: a MySQL server to a new one, MySQL to PostgreSQL, SQL Server or Oracle to MySQL, SQLite to PostgreSQL, MySQL or PostgreSQL into ClickHouse for analytics, BigQuery to anything, anything into or out of Snowflake or Databricks, lake tables from Trino into a database or a database into Iceberg, MongoDB to SQL and back, relational tables into Cassandra or ScyllaDB and wide rows out of them, tables into Elasticsearch or OpenSearch indices and documents back out, MySQL to TiDB, CockroachDB or YugabyteDB to PostgreSQL and back. Between servers of one family (MySQL and MariaDB, or PostgreSQL to PostgreSQL) definitions copy as they are; TiDB, CockroachDB and YugabyteDB lack parts of their family's dialect, so their tables are translated like any other engine's. Redis and Valkey hold keys rather than tables, so they aren't offered. Choose the source and target (a connection plus database or schema), then review the plan before anything runs.

- **The plan** lists every table and column with its source type and the type proposed on the target: `tinyint(1)` becomes `boolean`, `timestamptz` becomes `datetime(6)` in UTC, PostGIS geometry becomes MySQL `POINT SRID 4326` or Oracle `SDO_GEOMETRY` (registered with its SRID, so spatial indexes carry over; Oracle's SRID 8307 becomes EPSG 4326 elsewhere), a MongoDB `objectId` becomes `varchar(24)`, and so on. Anything that may lose information is flagged with the reason (a decimal without fixed precision, text shortened so it can be indexed, MongoDB dates keeping milliseconds). You can rename tables and columns, change types, leave out tables, columns, indexes and foreign keys, and preview the exact SQL for each table.
- **Same engine** copies types, defaults, computed columns, collations and comments as they are, and can also copy views, routines, triggers, custom types and sequences.
- **Across engines**, defaults are translated where there is an equivalent (`now()`, `CURRENT_TIMESTAMP(6)`, `GETDATE()`, `SYSDATE`; UUID generators; literals), enums become a native enum or a text column with a check, auto-numbering becomes the target's identity, and foreign key columns take the type of the key they reference. Check constraints, triggers and views written in the source's SQL are listed as not copied.
- **The run** drops tables you chose to replace, creates each table with its primary key, streams the rows in batches (committing every 20,000 rows), then adds indexes and foreign keys and moves identity counters past the copied keys. Parents are loaded before the tables that reference them. Progress is live, and a run can be stopped; the chunk in flight is rolled back.
- **Verification** counts the rows on both sides and, by default, reads the copy back and compares every column's values with the source after normalizing how each engine writes them (numbers, times and time zones, JSON, binary, intervals, and shapes compared by their coordinates). A difference names the columns involved.

Reading needs read access to the source; writing needs write access to the target. Replacing existing tables, and any migration into a production connection, asks you to type the target connection's name. Every run is kept with its report, and recorded in the audit log.

## Users and access

The Database users tab (connection menu, or the command palette) lists the server's users and roles. Pick one to see and change what it may do:

- **Access**: privileges by level, for the database and schema chosen at the top. Ticking one marks it to grant, unticking it to revoke. Nothing runs until you review the statements and apply them. Quick set replaces the account's access in that database or schema with a preset; Copy from… replaces it with another account's.
- **Account**: settings such as locked, connection limit or password expiry, role memberships, and a role's members.
- **As SQL**: everything granted to the account, as the server reports it.

What each engine offers:

| Engine | Accounts | Levels | Notes |
|---|---|---|---|
| PostgreSQL, CockroachDB, YugabyteDB | roles (users are roles that can log in) | database, schema, all tables, tables created later (default privileges), all sequences, table | Passwords are hashed to SCRAM-SHA-256 before they're sent. Dropping a role can first hand over what it owns and revoke its privileges (`REASSIGN OWNED`, `DROP OWNED`). |
| MySQL, MariaDB, TiDB | `user@host` accounts and roles | server, database, table | A database-wide grant shows as covering each table. Roles need MySQL 8 or MariaDB 10.0.5. TiDB has no connection limit. |
| SQL Server | logins and server roles | server, database, schema, table | A login gets a database user with its name the first time it's given access to a database. Membership of `db_datareader`, `db_datawriter` and `db_owner` shows as access through them. |
| ClickHouse | users and roles | server, database, table | A group grant such as `ALTER TABLE` shows on each privilege it includes, and unticking one revokes just that one. Accounts defined in the server's configuration files are shown as built in: SQL can't change them. |
| Oracle | users and roles | system privileges, schema (23ai), table | Plain new names are upper-cased, as Oracle does. Without the `DBA_` views, only grants on objects the connection's user owns show. |
| MongoDB | users and roles, named `user@db` | every database, database | Built-in roles (`read`, `readWrite`, `dbOwner`…) show as privileges; your own roles as memberships. |

Changes run on their own session, not the console: passwords never reach query history, and the audit log records the statements with passwords replaced by dots. Changes on production connections ask for confirmation, and read-only connections and people with read access can look but not change anything.

## Sharing and comments

The link button in a query tab (or **Share** on a saved query) creates a link to the query exactly as it is. Later edits in the tab don't change it, so line comments always point at the right code.

- **Who can open it**: everyone on the team, or chosen people, who are notified. Links only open for people signed in to your Rowsmith, and can expire after 1, 7 or 30 days. The author can change this or delete the link. Admins can open and delete any link.
- **Result snapshot**: optionally, Rowsmith runs the query once more on a read-only session and keeps up to 500 rows, sealed with the master key. Only people who can read that connection see the result or can open the query in a tab. Others see the SQL and the discussion. Sharing never grants data access.
- **Comments**: click a line number to start a thread on that line, or comment on the query as a whole. Comments support `inline code`, ```sql blocks```, links and @mentions (only of people who can see the link). The thread's author or the query's author can resolve a thread.
- **Notifications**: shares, mentions, replies in your threads and comments on your queries appear under the bell. When email is set up, they are also emailed, at most once every five minutes per query. Everyone can turn the emails off under **Account → Notifications**.

## AI assistant

An admin sets it up under **Administration → AI assistant** and turns it on for everyone. Each team uses its own account with a provider:

| Provider | Endpoint | Key | Notes |
|---|---|---|---|
| Anthropic | built in | [console.anthropic.com](https://console.anthropic.com/settings/keys) | Claude Opus, Sonnet or Haiku, with streamed reasoning, adjustable effort and a cached schema. `ANTHROPIC_API_KEY` in the environment also works. |
| OpenAI | `https://api.openai.com/v1` | [platform.openai.com](https://platform.openai.com/api-keys) | Any chat model; the list is loaded from your account. |
| OpenRouter | `https://openrouter.ai/api/v1` | [openrouter.ai/keys](https://openrouter.ai/keys) | Hundreds of models from many labs behind one key. |
| Custom | your URL | optional | Anything that speaks the OpenAI chat completions API: Ollama, LM Studio, vLLM, LiteLLM, llama.cpp, gateways and proxies. |

A Claude.ai or ChatGPT subscription can't be used here: providers only sell API access through API keys, billed per token.

**Self-hosted models.** From inside the container, a server on the same machine is at `host.docker.internal` (the compose file maps it). It has to listen beyond `127.0.0.1`. For Ollama, set `OLLAMA_HOST=0.0.0.0`, then use `http://host.docker.internal:11434/v1` as the endpoint. Models that can call tools give the best answers. With models that can't, Rowsmith still works: the assistant answers from the schema alone.

**What is sent.** The assistant gets the schema of the database in scope (names, types, keys and comments), what you have selected in the editor, and your question. It looks things up with read-only tools: listing tables, describing one, and checking a query against the database's planner without running it. If an admin allows it, it may also read a few sample rows and run small read-only queries. That can be limited to non-production connections. It never runs anything that changes data; you run what it writes yourself, with the usual production confirmations. Keys are encrypted with the master key, are never sent to the browser, and only ever go to the endpoint they were entered for. The audit log records each question's model and token counts, not its text.

## Install

Rowsmith runs as a container behind your reverse proxy. Each [release](https://github.com/abdulwaheedsyed/Rowsmith/releases) publishes the image `ghcr.io/abdulwaheedsyed/rowsmith` for linux/amd64 and linux/arm64, and the server on its own for Linux, macOS and Windows. The [changelog](CHANGELOG.md) lists what each release changes.

### With Docker Compose

[`deploy/compose.yaml`](deploy/compose.yaml) runs the image with a read-only root filesystem, all capabilities dropped, the master key as a Docker secret, and the port bound to `127.0.0.1` so only your reverse proxy can reach it. The image is distroless (about 85 MB) and runs as a non-root user.

```bash
curl -fsSLO https://raw.githubusercontent.com/abdulwaheedsyed/Rowsmith/main/deploy/compose.yaml
```

```bash
sudo install -d -m 700 /etc/rowsmith
```

```bash
openssl rand -base64 32 | sudo tee /etc/rowsmith/master.key >/dev/null && sudo chmod 444 /etc/rowsmith/master.key
```

```bash
echo 'ROWSMITH_PUBLIC_URL=https://db.example.com' | sudo tee /etc/rowsmith/compose.env
```

```bash
docker compose -f compose.yaml --env-file /etc/rowsmith/compose.env up -d
```

```bash
docker logs rowsmith 2>&1 | grep "setup code"
```

1. Download the compose file, or use `deploy/compose.yaml` in a clone of this repository.
2. Create the master key outside any web root, and back it up separately from the `rowsmith-data` volume.
3. Put the public URL in `/etc/rowsmith/compose.env`. A path such as `https://example.com/sql` works too. This starts the latest release. To stay on one release line and choose when to move to the next, also add `ROWSMITH_VERSION=0.1` to the file.
4. Start the container, then open the public URL and enter the setup code from the log to create the owner account.

### Upgrading

Read the release notes first: until 1.0, a new minor version may change how something works. Back up the `rowsmith-data` volume, then pull the new image and restart:

```bash
docker compose -f compose.yaml --env-file /etc/rowsmith/compose.env pull
```

```bash
docker compose -f compose.yaml --env-file /etc/rowsmith/compose.env up -d
```

Rowsmith updates its metadata database as it starts. To go back to an older version, restore the backup you made before upgrading.

### Without Docker

Download the archive for your system from the [releases](https://github.com/abdulwaheedsyed/Rowsmith/releases), with `SHA256SUMS`, and check it. The second check needs the [GitHub CLI](https://cli.github.com) and proves the archive was built by this repository's release workflow:

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

```bash
gh attestation verify rowsmith_0.1.0_linux_amd64.tar.gz --repo abdulwaheedsyed/Rowsmith
```

```bash
tar -xzf rowsmith_0.1.0_linux_amd64.tar.gz
```

```bash
ROWSMITH_DATA_DIR="$HOME/.rowsmith" ./rowsmith_0.1.0_linux_amd64/rowsmith serve
```

Then open `http://localhost:8080` and enter the setup code from the output. On Windows, unzip the archive, then in PowerShell, in its folder: `$env:ROWSMITH_DATA_DIR = "$HOME\.rowsmith"; .\rowsmith.exe serve`. Everything Rowsmith keeps goes in `ROWSMITH_DATA_DIR`, including a generated master key unless you supply one (see [Configuration](#configuration)). Sign-in cookies need HTTPS, or `localhost`: to serve other machines, put Rowsmith behind your reverse proxy and set `ROWSMITH_PUBLIC_URL`.

The container image can be checked the same way: `gh attestation verify oci://ghcr.io/abdulwaheedsyed/rowsmith:0.1.0 --repo abdulwaheedsyed/Rowsmith`.

### Building the image yourself

[`deploy/compose.build.yaml`](deploy/compose.build.yaml) builds the image from a clone instead of pulling a release, named after the commit it's built from:

```bash
ROWSMITH_BUILD_VERSION=$(git describe --tags --always --dirty --match 'v[0-9]*' | sed 's/^v//') docker compose -f deploy/compose.yaml -f deploy/compose.build.yaml --env-file /etc/rowsmith/compose.env up -d --build
```

### Reverse proxy and host databases

[`deploy/Caddyfile.example`](deploy/Caddyfile.example) shows Caddy on its own subdomain or under a path such as `/sql`. Keep `flush_interval -1` so query results stream. A subdomain is the stronger choice: under a shared hostname, other apps on that hostname share Rowsmith's browser origin.

To reach a database on the Docker host, use `host.docker.internal` as the host name. The database must listen on an address other than `127.0.0.1`. For a MySQL or MariaDB server that listens on `127.0.0.1` only, mount its socket directory instead (see the commented volume in `deploy/compose.yaml`) and enter the socket path, such as `/run/mysqld/mysqld.sock`, as the host. The server then sees a local login, so `user@localhost` accounts work.

### Command line

```bash
rowsmith create-user -email ada@example.com -name "Ada Lovelace" -role owner
rowsmith reset-password -email ada@example.com
rowsmith rotate-key
rowsmith version
```

Inside the container, run them with `docker exec`, for example `docker exec -it rowsmith /rowsmith reset-password -email …`.

## Development

You need Docker and Node 22.12 or later. Go runs inside a container, so no local toolchain is required.

```bash
dev/go.sh build -o /src/.bin/rowsmith ./cmd/rowsmith
```

```bash
docker compose -f dev/compose.yaml --env-file dev/.env up -d
```

```bash
python3 dev/smoke.py
```

```bash
cd web && npm install && npm run dev
```

- **Build**: the first command compiles the server into `.bin/rowsmith`.
- **Test databases**: `dev/compose.yaml` starts throwaway MySQL, MariaDB, TiDB, PostGIS, CockroachDB, YugabyteDB, ClickHouse, MongoDB, Redis and Valkey servers, an SSH bastion, and a PostgreSQL server that is only reachable through the bastion: everything the smoke test uses. Add `--profile full` to also start SQL Server, Oracle and a BigQuery emulator. Only Rowsmith is published, on `127.0.0.1:18080`.
- **Smoke test**: `dev/smoke.py` runs the end-to-end test against the dev stack.
- **Frontend**: `npm run dev` serves the UI at `http://127.0.0.1:5199` and proxies `/api` to the dev server.
- **Third-party notices**: after adding or upgrading dependencies, run `python3 dev/notices.py` to regenerate `THIRD_PARTY_NOTICES.txt`.

Create `dev/.env` with `DEV_DB_PASSWORD` and `DEV_SSH_PASSWORD` before starting the stack. It is ignored by git. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full set of checks, integration tests and how to send changes.

Unit tests:

```bash
dev/go.sh test ./...
```

### Adding a database engine

Create a package under `internal/driver/<engine>` that implements `driver.Driver`:

- `Info()` describes the connection form, capabilities and object kinds.
- `Open()` returns a `driver.Conn`.

Optional capabilities such as `Explainer`, `ProcessManager`, `DDLGenerator` and `Catalog` are separate interfaces; implement the ones the engine supports. Engines reachable through `database/sql` can reuse `sqlbase` for browsing, editing and streaming, and only supply a `Dialect`. Key-value stores set `Caps.Keys` and implement `KeyLister`, so the navigator finds keys on the server by pattern. Register the driver in `init()` and import the package from `internal/driver/all`.

## License

Rowsmith is released under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE). Rowsmith™ and the Rowsmith logo are trademarks of Syed Abdul Waheed; the license doesn't grant their use, except to describe this software.

It is built on open-source libraries under permissive licenses (MIT, BSD, Apache 2.0 and ISC, plus the MySQL driver under MPL 2.0), and bundles the Archivo and IBM Plex typefaces under the SIL Open Font License 1.1. Their licenses are in [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt), which also ships in the container image under `/licenses`.
