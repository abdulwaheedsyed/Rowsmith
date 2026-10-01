# Security policy

Rowsmith™ holds the keys to other people's databases, so security reports get priority over everything else.

## Reporting a vulnerability

Report it privately on GitHub: open the [Security tab](https://github.com/abdulwaheedsyed/Rowsmith/security), then **Report a vulnerability**. Don't open a public issue, pull request or discussion.

Include:

- what an attacker can do, and what access they need to start with (none, viewer, member, admin)
- the steps to reproduce it, ideally against a fresh instance
- the Rowsmith commit or version, the database engine involved, and how Rowsmith is deployed
- any proof of concept, logs or screenshots, with real secrets and data removed

## What happens next

- You'll get an acknowledgement within 5 working days, in the advisory thread.
- Within 14 days you'll get an assessment: whether it's a vulnerability, how severe it is, and the plan for a fix.
- Critical and high-severity issues are fixed first. You'll be kept up to date in the thread and can test the fix before it's released.
- Once a fix is available, a GitHub security advisory is published, with a CVE when warranted. You're credited in it unless you'd rather not be.

Please keep the details private until the advisory is published. If a fix is taking longer than 90 days from your report, we'll agree on a disclosure date together.

## Supported versions

Rowsmith is in early development and has no releases yet. Security fixes land on the `main` branch, so run an image built from the latest `main`. Once releases begin, the latest release will get security fixes.

## Scope

In scope is the Rowsmith server and web app in this repository, for example:

- signing in without valid credentials or a second factor, or taking over another session
- a viewer or member gaining admin or owner rights, or reaching a connection that wasn't shared with them
- reading saved secrets (database passwords, SSH keys, TLS keys, API keys) through the API, the browser or the audit log
- writing through a read-only session, or skipping the confirmation for changes on a production connection
- a shared query link revealing its SQL or result to someone outside its audience
- the AI assistant changing data, or seeing rows when its settings don't allow it
- a schedule's webhook reaching a private network address when an admin hasn't allowed that
- connecting through an SSH host whose key wasn't approved, or whose key changed
- cross-site scripting, cross-site request forgery, or claiming a new instance without the setup code
- a single request that crashes the server or exhausts its memory

Out of scope:

- vulnerabilities in the database servers themselves, or in dependencies with no demonstrated effect on Rowsmith (report those upstream, but do tell us if Rowsmith is affected)
- actions that need owner or admin rights, which are trusted with the instance's configuration, such as choosing which mail server or AI endpoint to use
- attacks that need control of the host, the data directory or the master key
- reports from automated scanners without a working proof of concept, and missing headers or cookie flags with no demonstrated impact
- denial of service by flooding requests, and social engineering
- the throwaway test servers in `dev/`

Rowsmith blocks statements it cannot prove are reads, but for guarantees against a determined user the [README](README.md#security-model) recommends read-only database credentials as well. A way past the read-only checks is still in scope.

## Testing safely

Test only against an instance you run yourself, with data you own. Don't access, change or keep other people's data. If you come across any, stop and include that in your report. Good-faith research that follows this policy is welcome, and won't be treated as an attack.

## Running Rowsmith securely

The [Security model](README.md#security-model) and [Deployment](README.md#deployment) sections of the README explain how secrets are protected and how to deploy safely. In short: serve Rowsmith over HTTPS behind your reverse proxy, back up the master key separately from the data directory, and give read-only users read-only database credentials.
