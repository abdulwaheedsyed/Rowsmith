# Rowsmith

**A fast, self-hosted web workspace for your databases.** Browse, query, edit and administer MySQL, MariaDB, PostgreSQL + PostGIS, SQL Server, Oracle, MongoDB, BigQuery and SQLite from one place, with built-in SSH tunnels, a team vault for connection secrets, and a UI designed for both desktop and phone.

Rowsmith is a single Go binary with the web app embedded. It is designed to run in its own container behind your existing reverse proxy.

> **Status: early development.** The backend core and the MySQL, MariaDB and PostgreSQL/PostGIS drivers are working and covered by an end-to-end test. The web UI, the remaining drivers and the collaboration features are being built now. See [Roadmap](#roadmap).

---

## Why Rowsmith

| | |
|---|---|
| **One workspace, many engines** | A common driver interface covers browsing, filtering, editing, SQL execution, EXPLAIN plans, process lists, variables and users. Each engine adds only what it supports, and new engines plug into the same registry. |
| **Built for teams** | Owner, admin, member and viewer roles. Share a connection with the whole team or with specific people, with read, write or manage access. Viewers are always read-only. |
| **Secrets handled properly** | Connection passwords, SSH keys, TLS keys and API keys are envelope-encrypted before they touch disk, and they are never sent back to the browser. |
| **Private servers, no extra software** | SSH tunnels, including chains of jump hosts, run inside the server process. New host keys must be approved once, and changed keys are rejected. |
| **Safety rails for production** | Statements are classified before they run. On production connections, anything that may modify data needs confirmation. `DROP`, `TRUNCATE`, and `DELETE` or `UPDATE` without `WHERE` always need it. Read-only access is enforced by the database session and by Rowsmith. |
| **An audit trail** | Sign-ins, connection changes, sharing, data edits and every data-modifying query are recorded with who, when and from where. |
| **Spatial data on a map** | PostGIS, MySQL and MariaDB geometry is decoded to GeoJSON, ready to show on a map. |

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

### Engines

| Engine | Status |
|---|---|
| MySQL 5.7 / 8.x / 9.x (incl. Percona, Aurora MySQL) | ✅ Working |
| MariaDB 10.x / 11.x | ✅ Working |
| PostgreSQL 10+ with PostGIS, TimescaleDB, Supabase, Neon, Aurora, AlloyDB, Cloud SQL | ✅ Working |
| Microsoft SQL Server / Azure SQL | 🚧 In progress |
| Oracle Database | 🚧 In progress |
| SQLite | 🚧 In progress |
| MongoDB | 🚧 In progress |
| Google BigQuery | 🚧 In progress |

Every driver is pure Go, so no Oracle Instant Client or Microsoft ODBC install is needed.

## Roadmap

- [x] Backend core: vault, auth, store, driver interface, SSH tunnels, API
- [x] MySQL, MariaDB and PostgreSQL/PostGIS drivers
- [ ] Web UI: connection manager, navigator, virtualized data grid, SQL editor with schema-aware autocomplete, structure editor, plan viewer, map view, command palette, mobile layout
- [ ] SQL Server, Oracle, SQLite, MongoDB and BigQuery drivers
- [ ] Import and export (SQL dump, CSV, JSON, XLSX)
- [ ] Relationship (ER) diagrams
- [ ] AI SQL assistant (Claude): write, explain and fix queries from your schema
- [ ] Scheduled queries and exports with email delivery and threshold alerts
- [ ] Production container image and reverse-proxy examples

## Architecture

```
cmd/rowsmith          entry point and CLI (serve, create-user, reset-password, rotate-key, healthcheck)
internal/api          JSON + NDJSON streaming API, auth middleware, CSRF, security headers
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
| `ROWSMITH_INSECURE_COOKIES` | `false` | Local plain-HTTP development only |

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
