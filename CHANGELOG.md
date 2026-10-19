# Changelog

The changes in each release of Rowsmith™. Versions follow [Semantic Versioning](https://semver.org/). Until 1.0, a new minor version (0.2, 0.3) may change how something works; its notes say what to do when upgrading.

## 0.5.0 - Unreleased

### Engines

- Redis Cluster: connecting to any node opens the whole cluster. Keys are listed from every primary, browsing and edits go to the node that owns each key, and processes show every node's clients. The console follows the cluster's redirects, runs `KEYS`, `DBSIZE` and `FLUSHDB` on every primary, and explains why `SELECT`, `SCAN` and transactions aren't available. A command on keys in different slots explains how hash tags keep keys together.
- Apache Cassandra (4.0 and later) and ScyllaDB (6 and later): keyspaces with their tables, materialized views, user types, functions and aggregates; the Structure tab with the partition key, clustering order, static columns, indexes and table options; values in their own types (user types, collections, tuples, vectors, exact decimals). The grid filters as Cassandra allows and sorts within a partition, and each edit is a lightweight transaction on the primary key, so an insert never overwrites a row. A CQL console with batches and cqlsh's USE, CONSISTENCY and TRACING; traced queries as plans; clients, settings and nodes; the structure editor; imports, exports, and migrations into and out of both.
- Elasticsearch (8 and 9) and OpenSearch (2 and 3) over their REST API, with passwords or API keys: indices, data streams and aliases with their mappings and settings; a grid that searches with typed filters, sorts by keyword sub-fields, puts geo fields on the map, and edits documents checked by sequence number; a console in Kibana Dev Tools syntax with hits, aggregations, `_cat`, SQL and ES|QL as tables; search profiles as plans; tasks you can cancel; settings and nodes; the structure editor; bulk imports, scrolled exports, and migrations into and out of both.
- The dev stack adds a three-node Redis Cluster, Cassandra, ScyllaDB, Elasticsearch and OpenSearch.

## 0.4.0 - 2026-10-19

### Engines

- Trino, with the lake tables behind it: catalogs shown with their connectors, Iceberg tables with their partitioning, sort order, snapshots and data files read from metadata (so counts and structure never scan data), Delta Lake tables with their partitions, properties, versions, files and size read from the transaction log, and Hive tables over Parquet, ORC and CSV files. Iceberg and Delta Lake rows are edited by matching all of their values, checked before any change is written, since both take one statement per transaction. Arrays, maps and rows show as JSON with their field names. The structure editor creates and changes tables with their format, partitioning and location. The console keeps `USE` and `SET SESSION`, and reads time travel and metadata tables. Distributed plans show as one tree, and Explain analyze adds rows and CPU time per operator. Running queries can be stopped, and session properties show as variables. Imports, exports, and migrations into Iceberg or Delta Lake or out of any Trino catalog are supported.
- The dev stack adds Trino with an Iceberg REST catalog, a Delta Lake catalog, SeaweedFS as S3 storage, and seeded Iceberg, Delta Lake, Parquet and CSV tables.

### Fixes

- A connection's configured schema (Snowflake, Trino) is now the one the navigator opens on.

## 0.3.0 - 2026-10-19

### Engines

- CockroachDB (24.1 and later, and CockroachDB Cloud), YugabyteDB (2.20 and later, and YugabyteDB Aeon) and TiDB (7.5 and later, and TiDB Cloud), each in the connection form with its own default port, through the PostgreSQL and MySQL drivers.
- CockroachDB: row counts from table statistics, keyless tables edited by their hidden rowid, its own plan tree, cluster sessions you can cancel, jobs, and cluster settings, all without the restricted crdb_internal tables. TLS is on by default.
- TiDB: TiDB's plans, schema changes applied one per statement, CHECK constraints when enforced, and grants read from its own tables.
- Migrations into and out of CockroachDB, YugabyteDB and TiDB, with types translated; TiDB stores spatial columns as GeoJSON.
- Snowflake: sign in with a key pair, a programmatic access token, an OAuth token or a password, through SSH tunnels too. The catalog, keys and DDL come from SHOW commands and GET_DDL, which never wake a warehouse. VARIANT, OBJECT and ARRAY columns are shown and edited as JSON, shapes go on the map, and numbers stay exact. The console handles $$ bodies and scripting blocks and refuses PUT and GET, which would touch files on the Rowsmith server. Plans with partition pruning, running queries you can cancel, session parameters and warehouses, users and roles, the structure editor (clustering keys and transient tables included), imports, exports, and migrations into and out of Snowflake.

### Fixes

- Migrating shapes from MySQL or MariaDB to a server without spatial types (PostgreSQL without PostGIS) stored each one as a quoted JSON string instead of a GeoJSON object.

## 0.2.0 - 2026-10-19

### Engines

- Redis and Valkey. The navigator lists keys by type and finds them on the server by name or pattern (`user:*`), and each numbered database shows its key count. Opening a key shows a grid for its type: string, hash, list, set, sorted set, stream or JSON document. You can filter, search, sort and edit it, and a save is checked first, then applied in one transaction that fails if the key changed meanwhile.
- New keys of any type, and expiry times, from dialogs that show their commands. A key's view shows when it expires.
- A console in redis-cli syntax with replies laid out as tables. `SELECT` and `MULTI` carry between runs, a blocking command can be cancelled, and `FLUSHDB`, `FLUSHALL` and `SHUTDOWN` always ask first.
- Password or ACL user sign-in, TLS, SSH tunnels, `redis://` and `valkey://` URLs, the client list and `INFO`/`CONFIG`.

### Fixes

- Editing a connection uses the dialog's full width again. The form had been squeezed into the narrow column that the database list fills when you create a connection.

## 0.1.0 - 2026-10-19

The first release: a container image for linux/amd64 and linux/arm64, and the server for Linux, macOS and Windows.

### Engines

- MySQL 5.7 to 9.x, MariaDB 10 and 11, PostgreSQL 10+ with PostGIS, SQL Server 2012+ and Azure SQL, Oracle 12c+, ClickHouse 24.8+ and ClickHouse Cloud, SQLite 3, MongoDB 5+ and Google BigQuery. Every driver is pure Go, so no client libraries need installing.

### Working with data

- A data browser with server-side filters, quick search, sorting, paging and row counts.
- Row editing in batches. Each change must match exactly one row, or nothing is saved.
- A SQL console with streamed results, script splitting for each engine's dialect, transactions that stay open across runs, and cancelling.
- Query plans as a tree, spatial columns on a map, and relationship diagrams.
- Import from CSV, TSV, JSON, NDJSON and Excel. Export to the same formats and to SQL dumps.

### Managing databases

- A structure editor for tables and collections that shows the SQL as you edit.
- Database users, roles and privileges, managed without writing GRANT statements.
- Running processes, server variables, and database and table sizes.
- Migration between servers and across engines, with every column type mapped for review and the copy checked value by value.

### Teams

- Owner, admin, member and viewer roles. Connections can be shared with read, write or manage access.
- Connection secrets encrypted at rest, and SSH tunnels through jump hosts.
- Two-step sign-in, an audit log, and confirmations for changes on production connections.
- Saved queries and history, shared query links with line comments, and scheduled queries delivered by email or webhook, or sent only when an alert's condition is met.
- An AI assistant using Anthropic, OpenAI, OpenRouter or a self-hosted model.
