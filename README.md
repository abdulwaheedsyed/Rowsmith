# Rowsmith

**A fast, self-hosted web workspace for your databases.** Browse, query, edit and administer MySQL, MariaDB, PostgreSQL + PostGIS, SQL Server, Oracle, MongoDB, BigQuery and SQLite from one place, with built-in SSH tunnels, a team vault for connection secrets, and a UI designed for both desktop and phone.

Rowsmith is a single Go binary with the web app embedded. It is designed to run in its own container behind your existing reverse proxy.

> **Status: early development.** All eight engines, the web UI, SSH tunnels, the team vault, the structure editor, import and export, the AI assistant, and scheduled queries are working. MySQL, MariaDB, PostgreSQL/PostGIS and MongoDB are covered by an end-to-end test; SQL Server, Oracle, SQLite and BigQuery have unit tests and integration tests you can run against real servers. Shareable query links and comments are next. See [Roadmap](#roadmap).

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
| **Spatial data on a map** | PostGIS, MySQL and MariaDB geometry is decoded to GeoJSON, ready to show on a map. |
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
- **Structure editor**: create and change tables (or MongoDB collections) from a form, with the exact SQL for your engine shown live as you edit. Columns can be renamed, reordered where the engine allows it, typed, keyed and computed; indexes, foreign keys, checks and table options are edited in place. Each engine only offers what it can do, and dropping columns asks first. Also: new databases and schemas, and renaming objects.
- **Export**: tables (with the grid's filters and sort), query results and whole schemas to CSV, TSV, JSON, NDJSON, Excel or SQL INSERT statements, optionally gzipped. Files are prepared on the server with live progress, so exports are not limited to the rows on screen.
- **SQL dumps**: a script that recreates tables, rows, keys, views, routines and triggers, restorable with the database's own client (`mysql`, `psql`) or with Rowsmith. Handles partitions, sequences and identity columns, and leaves out objects that extensions such as PostGIS create themselves.
- **Import**: CSV (delimiter detected), TSV, JSON, NDJSON and Excel into an existing table, with a preview and column mapping, in one transaction: if a row fails, nothing is kept. SQL files run statement by statement with the console's safety rules.
- **EXPLAIN / EXPLAIN ANALYZE** turned into a plan tree. PostgreSQL's `ANALYZE` runs inside a transaction that is always rolled back.
- **Administration**: running processes (with kill), server variables and status, database users and grants
- **Type fidelity**: exact decimals, 64-bit integers, binary previews with image detection, and dates exactly as the server stores them
- **Accounts**:
  - Argon2id password hashing
  - two-step sign-in (TOTP) with recovery codes
  - account lockout and rate limiting
  - session management
  - first-run setup protected by a one-time code
- **Query history and saved queries** (private or shared with the team), plus notes on connections and objects
- **Scheduled queries**: run a query every few minutes, hourly, on chosen weekdays or monthly (or with a cron expression), in any time zone. Send the result by email (attached, with a preview in the message) or to a webhook, or alert only when a condition is met. Each run is kept with its file for download. See [Schedules](#schedules).
- **AI assistant** in every query tab (`Ctrl+I`): write queries from a description, explain them, fix a failed statement from its error, or speed one up from its plan. Answers stream with SQL you can insert, replace or run. See [AI assistant](#ai-assistant).

### Engines

| Engine | Status |
|---|---|
| MySQL 5.7 / 8.x / 9.x (incl. Percona, Aurora MySQL) | ✅ Working |
| MariaDB 10.x / 11.x | ✅ Working |
| PostgreSQL 10+ with PostGIS, TimescaleDB, Supabase, Neon, Aurora, AlloyDB, Cloud SQL | ✅ Working |
| Microsoft SQL Server 2012+ / Azure SQL | ✅ Working |
| Oracle Database 12c+ | ✅ Working |
| SQLite 3 | ✅ Working |
| MongoDB 5+ | ✅ Working |
| Google BigQuery | ✅ Working |

Every driver is pure Go, so no Oracle Instant Client or Microsoft ODBC install is needed.

**Engine notes**

- **SQL Server**: `GO` batches, `PRINT` and `RAISERROR` messages as notices, showplan XML turned into a plan tree, read-only application intent for replicas, and `geometry`/`geography` on the map.
- **Oracle**: connect by service name or SID, optional `SYSDBA`-style roles, PL/SQL blocks split on `/`, `DBMS_OUTPUT` shown as notices, and packages, synonyms and sequences in the navigator.
- **SQLite**: databases are files on the Rowsmith server, so the driver only opens files inside `ROWSMITH_SQLITE_DIR`. `ATTACH` is disabled and Rowsmith's own metadata database is refused. Use `:memory:` for a scratch database.
- **MongoDB**: a console that understands mongosh syntax (`db.orders.find({...}).sort(...)`, `aggregate`, CRUD, index and collection commands) without running JavaScript on the server. Field lists are inferred from a sample of documents, and edits keep BSON types.
- **BigQuery**: sign in with a service-account key or Application Default Credentials. Every query is dry-run first; queries estimated to process more than the connection's limit (10 GB by default) are refused, and each job carries the limit. Table previews use the free `tabledata.list` API. The service account needs `roles/bigquery.jobUser` on the project and `roles/bigquery.dataViewer` (or `dataEditor`) on the datasets.

## Roadmap

- [x] Backend core: vault, auth, store, driver interface, SSH tunnels, API
- [x] MySQL, MariaDB, PostgreSQL/PostGIS, SQL Server, Oracle, SQLite, MongoDB and BigQuery drivers
- [x] Web UI: connection manager, navigator, virtualized data grid, SQL editor with schema-aware autocomplete, plan viewer, map view, command palette, mobile layout
- [x] Relationship (ER) diagrams
- [x] Production container image and reverse-proxy examples
- [x] Structure editor for every engine
- [x] Import and export (SQL dump, CSV, TSV, JSON, NDJSON, Excel)
- [x] AI SQL assistant: write, explain, fix and speed up queries from your schema, with Anthropic, OpenAI, OpenRouter or a self-hosted model
- [x] Scheduled queries and exports with email and webhook delivery and threshold alerts
- [ ] Shareable query links and comments

## Architecture

```
cmd/rowsmith          entry point and CLI (serve, create-user, reset-password, rotate-key, healthcheck)
internal/api          JSON + NDJSON streaming API, auth middleware, CSRF, security headers
internal/ai           SQL assistant: Anthropic and OpenAI-compatible providers, read-only schema tools
internal/schedule     scheduled queries: cron timetables, alerts, encrypted result files, email and webhook delivery
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
dev/                  throwaway test databases, SSH bastion, smoke test
```

The frontend is compiled into the Go binary. Rowsmith keeps its own data (users, connections, history) in a SQLite file in the data directory. It needs no external database.

## Security model

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

## Deployment

The `Dockerfile` builds a small distroless image (about 85 MB) that runs as a non-root user. [`deploy/compose.yaml`](deploy/compose.yaml) runs it with a read-only root filesystem, all capabilities dropped, the master key as a Docker secret, and the port bound to `127.0.0.1` so only your reverse proxy can reach it.

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
docker compose -f deploy/compose.yaml --env-file /etc/rowsmith/compose.env up -d --build
```

```bash
docker logs rowsmith 2>&1 | grep "setup code"
```

1. Create the master key outside any web root, and back it up separately from the `rowsmith-data` volume.
2. Put the public URL in `/etc/rowsmith/compose.env` (a path such as `https://example.com/sql` works too), then build and start the container.
3. Open the public URL and enter the setup code from the log to create the owner account.

[`deploy/Caddyfile.example`](deploy/Caddyfile.example) shows Caddy on its own subdomain or under a path such as `/sql`. Keep `flush_interval -1` so query results stream. A subdomain is the stronger choice: under a shared hostname, other apps on that hostname share Rowsmith's browser origin.

To reach a database on the Docker host, use `host.docker.internal` as the host name. The database must listen on an address other than `127.0.0.1`. For a MySQL or MariaDB server that listens on `127.0.0.1` only, mount its socket directory instead (see the commented volume in `deploy/compose.yaml`) and enter the socket path, such as `/run/mysqld/mysqld.sock`, as the host. The server then sees a local login, so `user@localhost` accounts work.

### Command line

```bash
rowsmith create-user -email ada@example.com -name "Ada Lovelace" -role owner
rowsmith reset-password -email ada@example.com
rowsmith rotate-key
```

Inside the container, run them with `docker exec`, for example `docker exec -it rowsmith rowsmith reset-password -email …`.

## Development

You need Docker and Node 20.19+. Go runs inside a container, so no local toolchain is required.

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
- **Test databases**: `dev/compose.yaml` starts throwaway MySQL, MariaDB and PostGIS servers, an SSH bastion, and a PostgreSQL server that is only reachable through the bastion. Add `--profile full` to also start SQL Server, Oracle, MongoDB and a BigQuery emulator. Only Rowsmith is published, on `127.0.0.1:18080`.
- **Smoke test**: `dev/smoke.py` runs the end-to-end test against the dev stack.
- **Frontend**: `npm run dev` serves the UI at `http://127.0.0.1:5199` and proxies `/api` to the dev server.

Create `dev/.env` with `DEV_DB_PASSWORD` and `DEV_SSH_PASSWORD` before starting the stack. It is ignored by git.

Unit tests:

```bash
dev/go.sh test ./...
```

### Adding a database engine

Create a package under `internal/driver/<engine>` that implements `driver.Driver`:

- `Info()` describes the connection form, capabilities and object kinds.
- `Open()` returns a `driver.Conn`.

Optional capabilities such as `Explainer`, `ProcessManager`, `DDLGenerator` and `Catalog` are separate interfaces; implement the ones the engine supports. Engines reachable through `database/sql` can reuse `sqlbase` for browsing, editing and streaming, and only supply a `Dialect`. Register the driver in `init()` and import the package from `internal/driver/all`.

## License

To be decided.
