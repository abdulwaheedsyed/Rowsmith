# Contributing to Rowsmith

Thanks for helping. Bug reports, fixes, documentation and support for new engines are all welcome. For anything bigger than a small fix, open an issue first so we can agree on the approach before you spend time on it.

Everyone taking part in the project follows the [code of conduct](CODE_OF_CONDUCT.md).

## Reporting a bug

[Open an issue](https://github.com/abdulwaheedsyed/Rowsmith/issues/new/choose) with the bug report form. It asks for the Rowsmith version and commit, how you run it, the databases involved, the steps that cause the problem, and what you expected. For ideas, use the feature request form.

Remove passwords, connection strings, host names and data you can't share before you post logs or screenshots.

## Reporting a security problem

Don't open a public issue. Report it privately, as described in the [security policy](SECURITY.md).

## Setting up

You need Docker, Node 20.19 or later, and Python 3 for the dev scripts. Go runs in a container through `dev/go.sh`, so you don't need to install it.

1. Create `dev/.env` with the passwords for the test servers. Git ignores this file.

   ```
   DEV_DB_PASSWORD=choose-a-password
   DEV_SSH_PASSWORD=choose-another
   ```

2. Build the web app and the server into `.bin/rowsmith`:

   ```bash
   ./dev/build.sh
   ```

3. Start Rowsmith with throwaway MySQL, MariaDB and PostGIS servers, an SSH bastion, and a PostgreSQL server reachable only through the bastion:

   ```bash
   docker compose -f dev/compose.yaml --env-file dev/.env up -d
   ```

   Add `--profile full` to also start SQL Server, Oracle, MongoDB and a BigQuery emulator. Oracle takes a few minutes the first time.

4. Open `http://127.0.0.1:18080`. The first-run setup code is in the container's log (`docker logs rowsmith-dev-rowsmith-1`), or let the smoke test create the owner account for you.

After changing code, run `./dev/build.sh` again, then `docker restart rowsmith-dev-rowsmith-1`. For the UI with hot reload, run `npm run dev` in `web/` and open `http://127.0.0.1:5199`; it sends `/api` requests to the dev server.

The [Architecture](README.md#architecture) section of the README explains how the code is laid out, and [Adding a database engine](README.md#adding-a-database-engine) covers new drivers.

## Before you open a pull request

Run the checks that apply to your change:

```bash
dev/go.sh fmt ./...
```

```bash
dev/go.sh vet ./...
```

```bash
dev/go.sh test ./...
```

```bash
cd web && npm run typecheck
```

```bash
python3 dev/smoke.py
```

The smoke test runs end to end against the dev stack on MySQL, MariaDB, PostGIS and MongoDB: first-run setup and sign-in, connections and SSH tunnels, browsing, editing, queries and plans, read-only and production safeguards, viewer permissions and the audit log.

### Integration tests

Driver changes for SQL Server, Oracle, MongoDB and BigQuery need their integration tests, which run against the `--profile full` servers. They skip unless their variables are set. For example, for Oracle:

```bash
source dev/.env && NET=rowsmith-dev_dev GO_ENV="-e ROWSMITH_TEST_ORACLE_HOST=oracle -e ROWSMITH_TEST_ORACLE_USER=shop -e ROWSMITH_TEST_ORACLE_PASSWORD=$DEV_DB_PASSWORD" dev/go.sh test ./internal/driver/oracle/
```

| Engine | Variables, with the dev stack's values |
|---|---|
| SQL Server | `ROWSMITH_TEST_MSSQL_HOST=mssql`, `ROWSMITH_TEST_MSSQL_PASSWORD=${DEV_DB_PASSWORD}Aa1!` |
| Oracle | `ROWSMITH_TEST_ORACLE_HOST=oracle`, `ROWSMITH_TEST_ORACLE_USER=shop`, `ROWSMITH_TEST_ORACLE_PASSWORD=$DEV_DB_PASSWORD` |
| MongoDB | `ROWSMITH_TEST_MONGO_HOST=mongo`, `ROWSMITH_TEST_MONGO_PASSWORD=$DEV_DB_PASSWORD` |
| BigQuery | `ROWSMITH_TEST_BQ_ENDPOINT=http://bigquery:9050` |

MySQL, MariaDB, PostgreSQL and MongoDB are also covered by the smoke test.

## Writing code

- **Match the code around you**: its naming, comment density and idioms. Comments explain why, not what.
- **Go**: keep to the standard library where it does the job. Errors that reach people should say what went wrong and what to do about it. Add or extend unit tests with each change.
- **Web app**: TypeScript in strict mode. Use the shared components in `web/src/components/ui.tsx` and the colour tokens in `web/src/styles/tokens.css` rather than fixed colours, so the light and dark themes both work. Check your change in both themes and at phone width, with the keyboard, and with reduced motion turned on.
- **Words in the UI**: use sentence case and plain words. Name controls by what they do ("Save changes", not "Submit"), and keep the same word through a flow. Error messages say what happened and how to fix it, without apologising.
- **Security**: secrets never go back to the browser, and data-changing statements on production connections need confirmation. Changes to sign-in, the vault, sharing or statement checks need tests that show the protection still holds. See the [Security model](README.md#security-model).
- **Documentation**: update the README when you change behaviour, configuration or a command.

## Dependencies

Add a dependency only when it saves real work. Its license must be permissive (MIT, BSD, ISC, Apache 2.0, or MPL 2.0 for libraries used unmodified). GPL, LGPL and AGPL code can't go in, because Rowsmith ships as a single Apache-2.0 binary.

After adding, removing or upgrading a dependency, regenerate the third-party notices and commit the result:

```bash
python3 dev/notices.py
```

## Commits and pull requests

- **Keep each pull request to one change**, so it can be reviewed and reverted on its own.
- **Write commit summaries in the imperative**, in sentence case and without a full stop, saying what changes for the people using Rowsmith. For example: "Add scheduled queries with email, webhook and alert delivery" or "Keep huge values from exhausting server memory". Use the body to explain why, when that isn't obvious.
- **In the pull request**, fill in the template: what changed and why, how you tested it (including which engines), and, for UI changes, screenshots in light and dark.

## License

Rowsmith is licensed under the [Apache License 2.0](LICENSE). Under section 5 of the license, anything you submit for inclusion is licensed under the same terms, and you confirm you have the right to submit it. There's no separate contributor agreement to sign.
