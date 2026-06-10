# Governance

Facet Architecture is an open specification and reference implementation,
MIT-licensed.

## Decision-making

- **Day-to-day** changes (bug fixes, stdlib facets, docs) merge on one maintainer
  review with a green CI.
- **Language/spec changes** (FDL grammar, primitive semantics, the `fa` public
  API) require an **ADR** in `DECISIONS.md` and a maintainer consensus. Breaking
  changes need a migration path.
- **Security** issues are handled privately first (see `SECURITY.md`), then
  disclosed with the fix.

## Roles

- **Contributors** — anyone who opens a PR/issue.
- **Maintainers** — review and merge; stewards of the spec and the public API.

## Stability

- The `fa` package and FDL follow semantic versioning once `v1.0.0` ships.
- A facet's `facet-id` pattern and required `what:` props are part of its public
  API; changing them is a major version bump (enforced for stdlib and the
  registry).

## Roadmap & priorities

Tracked in `README.md` ("Known gaps & roadmap"). The north star: a
server-authoritative framework enterprises can bet a product on — correct,
secure, operable, and ergonomic.
