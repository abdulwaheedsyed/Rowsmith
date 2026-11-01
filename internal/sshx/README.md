# internal/sshx

A copy of [golang.org/x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh) with SSH compression added, which the original leaves out. Rowsmith's SSH tunnels use it, so a tunnel can compress database traffic for protocols that can't compress it themselves, such as PostgreSQL's.

- `UPSTREAM` names the x/crypto version it was copied from: the one in `go.mod`.
- `rowsmith.patch` holds the changes to upstream files: a `Compressions` setting in `Config`, and the hooks in the transport.
- Files named `rowsmith_*` are Rowsmith's own: `zlib` and `zlib@openssh.com` compression (RFC 4253, section 6.2), a zlib decoder that works one packet at a time, and their tests. `TestIntegrationOpenSSHCompression` runs against the dev stack's OpenSSH bastion.
- Everything else is upstream code under the Go authors' BSD license (`LICENSE`, `PATENTS`), with its import paths changed. Upstream's tests and the `agent`, `knownhosts` and `terminal` packages are left out.

Don't edit the upstream files by hand without saving the change into the patch. `dev/sshx-sync.sh` rebuilds the directory from the module cache and the patch, and `dev/sshx-sync.sh save` writes your edits to the upstream files into the patch. CI rebuilds it and fails when the result differs from the committed copy, so when golang.org/x/crypto is updated (by Dependabot too), run the script and commit the result: that's how upstream fixes reach this copy. If the patch no longer applies, redo the hooks on the new upstream files and save them.

govulncheck can't see vulnerabilities in this copy, since Rowsmith no longer calls x/crypto's SSH package. A Dependabot alert for golang.org/x/crypto still applies to it.

As with any compression before encryption, the size of compressed packets says something about their content: someone who can both put data into the traffic and watch packet sizes on the network may learn about other data sent with it (as in the CRIME attack on TLS). That's why compression is off unless a connection turns it on.
