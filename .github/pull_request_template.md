## What and why

<!-- What changes for people using Rowsmith, and why. Link the issue it fixes, e.g. "Fixes #123". -->

## How you tested it

<!-- The checks you ran and the databases you tried it on. Delete the lines that don't apply. -->

- [ ] `dev/go.sh vet ./...` and `dev/go.sh test ./...`
- [ ] `npm run typecheck` in `web/`
- [ ] `python3 dev/smoke.py` against the dev stack
- [ ] Integration tests for the drivers I changed
- Databases: <!-- e.g. MySQL 8.4, PostgreSQL 16 -->

## Screenshots

<!-- For changes to the UI: before and after, in light and dark. Delete this section otherwise. -->

## Checklist

- [ ] Tests cover the change, including the protections it touches (sign-in, the vault, sharing, statement checks).
- [ ] The README is updated if behaviour, configuration or a command changed.
- [ ] `python3 dev/notices.py` was run if dependencies changed.
- [ ] Nothing in this pull request contains real passwords, keys or private data.
