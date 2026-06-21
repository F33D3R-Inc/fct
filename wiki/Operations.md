# Operations

Running Facet for real: deploying, scaling horizontally, observing it, durable
background work, and backups. Everything here is a runtime service through the
same IR — no extra moving parts unless you turn them on.

- [Deploying](#deploying)
- [Horizontal scale (clustering)](#horizontal-scale-clustering)
- [Durable jobs](#durable-jobs)
- [Observability](#observability)
- [Resilience](#resilience)
- [Backup & restore](#backup--restore)
- [Supply chain](#supply-chain)

---

## Deploying

`facet deploy app.fct` writes a `Dockerfile`, `.dockerignore`,
`docker-compose.yml`, and `.env.example` (also created by `facet new`). The
compose file brings up the app **and** its Postgres:

```sh
cp .env.example .env                 # set FACET_SECRET (facet config --gen-secret)
docker compose up --build
```

In production set, at minimum:

```sh
FACET_DATABASE_URL=postgres://user:pw@host:5432/db
FACET_SECRET=<32+ bytes>             # facet config --gen-secret
FACET_SECURE_COOKIES=1               # behind TLS
```

`facet run` reconciles the schema on startup, serves the hardened projections,
and shuts down gracefully on SIGTERM. Run `facet config` to see the resolved
configuration and warnings.

## Horizontal scale (clustering)

Set `FACET_CLUSTER=1` and run several stateless `facet run` instances behind a
load balancer. They cooperate with **no extra infrastructure**:

- **Sessions** live in the shared Postgres store, so any instance can serve any
  request.
- **Live updates** ride **Postgres `LISTEN`/`NOTIFY`** — the database you already
  run is the cross-instance bus. An entity change on one instance fans out over
  SSE to clients connected to every instance.

A single-process dev run (no flag) keeps the fast in-memory path.

## Durable jobs

With clustering, the in-memory job ticker is replaced by a **persistent queue**:

- every `every Ns` job becomes a **cron entry**; on each interval exactly one
  instance wins the reservation and enqueues a row, so the job fires **once
  across the cluster**, not once per instance;
- a failed job is **retried with exponential backoff** (capped), and once it
  exhausts its attempts it is **dead-lettered** (kept for inspection) rather than
  silently lost.

Define jobs in the language — see
[Actions & Logic → Jobs](Actions-and-Logic.md#jobs).

## Observability

| Surface | Detail |
|---|---|
| **Structured logs** | JSON via `slog`; level from `FACET_LOG_LEVEL` (`debug`/`info`/`warn`/`error`) |
| **Metrics** | Prometheus exposition at `GET /metrics` |
| **OTLP logs** | export with `FACET_OTLP_LOG` |
| **Liveness** | `GET /healthz` |
| **Readiness** | `GET /readyz` (checks the database) |

Point Prometheus at `/metrics` and your orchestrator's probes at
`/healthz` + `/readyz`.

## Resilience

- **Graceful shutdown** — on SIGTERM/SIGINT, in-flight requests drain (up to
  ~25s), job workers stop, and the database closes cleanly. Deploy-safe.
- **Timeouts** — read-header and idle timeouts stop a slow client from pinning a
  connection (the SSE stream is intentionally exempt).
- **API micro-cache** — set `FACET_API_CACHE_TTL` for a short-TTL cache in front
  of read endpoints under load.

## Media handoff

The `upload` node names a runtime media service configured from the environment.
By default a stored file is served from an unguessable public path under
`/uploads/`. Three operational capabilities build on that:

- **Signed, expiring URLs** — set `FACET_MEDIA_TTL=<seconds>` and every stored
  file is served from a signed, time-limited link (an HMAC over name + expiry,
  keyed by the master secret). An unsigned, tampered, or expired link is **403**,
  so media is access-controlled, not permanently public. With the TTL unset,
  links stay public (backward compatible).
- **Resumable chunked upload** — a file bigger than one request
  (`FACET_MAX_UPLOAD_MB`, default 10) is sent in pieces (`/upload/init` →
  `/upload/chunk` → `/upload/finish`) and reassembled server-side, up to
  `FACET_MAX_MEDIA_MB` (default 200). The browser `upload` node switches to this
  automatically for large files; small ones still go in a single POST.
- **HLS delivery** — `.m3u8`/`.ts`/`.m4s`/`.mpd` are served with their stream MIME
  types and byte ranges (so a player seeks cleanly). When signing is on, an
  `.m3u8` playlist is rewritten on the fly so each segment URI carries its own
  signature, so a protected stream still plays. Segments are flat siblings of the
  playlist in the upload dir; transcoding/segmenting is done outside fct (e.g.
  ffmpeg) and the resulting bundle is uploaded.

`facet config` reports the media mode. A runnable demo is in `examples/media.fct`.

## Backup & restore

Logical snapshots, independent of `pg_dump`:

```sh
facet backup  app.fct > snapshot.json     # or: facet backup app.fct snapshot.json
facet restore app.fct < snapshot.json     # or: facet restore app.fct snapshot.json
```

## Supply chain

The release workflow (triggered by pushing a `v*` tag) produces a verifiable
release:

- a **CycloneDX SBOM** (`scripts/sbom.sh`);
- **keyless signatures** over the checksums, SBOM, and provenance via **cosign**
  (Sigstore), using the workflow's OIDC identity — no long-lived signing key;
- a **SLSA v1.0 provenance** statement (`scripts/provenance.sh`).

Verify a download:

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/.+/.github/workflows/release.yml@.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt
```

→ See **[Configuration](Configuration.md)** for every environment variable.
