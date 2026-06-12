# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Report privately, either way:

- **GitHub:** [Report a vulnerability](https://github.com/F33D3R-Inc/fct/security/advisories/new)
  (private security advisory — preferred), or
- **Email:** eddapp@zoho.com with subject line `[SECURITY] fct: <summary>`.

Include what you can: affected version (`fct version`), a reproduction or
proof of concept, and impact as you understand it.

### What to expect

- **Acknowledgement** within 72 hours.
- **Assessment** and severity within 7 days.
- **Coordinated disclosure:** we ask that you keep the report private until a
  fix ships; we'll credit you in the release notes unless you prefer otherwise.
  Fixes for confirmed vulnerabilities are released as soon as they're ready —
  pre-1.0, that means a patch release of the latest minor.

## Supported versions

Pre-1.0, only the **latest minor release** receives security fixes. Run the
newest `v0.x` and subscribe to releases.

## Scope

In scope: the `fct` compiler/CLI, the `fa` server library, the web runtime
(`fa-runtime.js`), the native runtimes (`clients/swift`, `clients/android`),
`std`, `fatest`, and the package registry tooling. Out of scope: applications
built with FA (report to their authors), and issues requiring a compromised
host.

## Security design

FA is secure-by-default by design: scoped SSE delivery, structural `who:`
authorization, CSRF protection, rate limiting, CSP, contextual auto-escaping,
and HMAC-signed events verified on every client runtime.

- Architecture-level summary: [ARCHITECTURE.md](ARCHITECTURE.md#security-architecture)
- The full red-team audit, threat model, and finding-by-finding status:
  [SECURITY_AUDIT.md](SECURITY_AUDIT.md)
