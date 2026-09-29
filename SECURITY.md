# Security Policy

## Reporting a Vulnerability

If you find a security vulnerability in recall, please report it privately
instead of opening a public issue.

- Email: open an issue marked **private/security**, or contact the maintainer
  via the email listed on the [maintainer's GitHub profile](https://github.com/pratikgajjar).
- Please include a description of the vulnerability, steps to reproduce, and
  the version or commit you tested against.

We will acknowledge receipt within 72 hours and aim to ship a fix or
mitigation within 14 days, then disclose once the fix is released.

## Scope

recall is a local-first tool: it reads AI session transcripts already on your
machine into a local SQLite index and never sends your data anywhere. The
security posture that matters most here:

- the index and its excerpts stay on disk, read-only with respect to sources;
- no network calls are made by the `recall` CLI itself;
- the pi extension shells out to the local binary only.

If you believe any of these invariants is broken, that is in scope and we
want to hear about it.
