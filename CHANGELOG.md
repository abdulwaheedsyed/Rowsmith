# Changelog

The changes in each release of Rowsmith™. Versions follow [Semantic Versioning](https://semver.org/). Until 1.0, a new minor version (0.2, 0.3) may change how something works; its notes say what to do when upgrading.

## 0.1.0 - Unreleased

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
