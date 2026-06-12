# Deployment

An FA app compiles to **one static Go binary** that serves everything — pages,
the SSE stream, the event endpoint, the runtime JS. Deployment is "run the
binary"; this page covers doing that properly.

## The two production requirements

Before anything else, know these — they're the only FA-specific rules:

1. **Set a stable signing key.** Pushed fragments are HMAC-signed; the key
   must be identical on every instance and survive redeploys:

   ```sh
   export FA_SIGNING_KEY=$(openssl rand -hex 32)   # generate ONCE, store in your secret manager
   ```

   Without it, each process generates a random ephemeral key — fine in dev
   (you'll see a warning), broken across instances and restarts.

2. **SSE needs sticky load-balancing** when you run more than one instance:
   a client's `/sse` and `/events` must land on the same instance (session
   affinity / `ip_hash`). Cross-instance delivery is the broker's job (below).

## Docker

`fct new` already wrote a production `Dockerfile` (distroless, static binary):

```sh
docker build -t myapp .
docker run -p 7373:7373 -e FA_SIGNING_KEY=$FA_SIGNING_KEY myapp
```

## docker-compose: app + Postgres + Redis

The full local-prod stack matching
[Working with Databases](Working-with-Databases.md):

```yaml
services:
  app:
    build: .
    ports: ["7373:7373"]
    environment:
      FA_ADDR: ":7373"
      FA_SIGNING_KEY: ${FA_SIGNING_KEY:?run: export FA_SIGNING_KEY=$(openssl rand -hex 32)}
      DATABASE_URL: postgres://app:secret@db:5432/app?sslmode=disable
      REDIS_ADDR: redis:6379
    depends_on:
      db: { condition: service_healthy }
      redis: { condition: service_started }

  db:
    image: postgres:17
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: app
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app"]
      interval: 5s
      retries: 10

  redis:
    image: redis:7-alpine

volumes:
  pgdata:
```

```sh
export FA_SIGNING_KEY=$(openssl rand -hex 32)
docker compose up --build
```

(Redis is only needed once you run **multiple** app instances — harmless to
have it ready.)

## Reverse proxy (TLS + SSE settings)

SSE is plain HTTP, but proxies buffer by default, which breaks streaming.

**Caddy** (easiest — automatic HTTPS, streams SSE correctly out of the box):

```
example.com {
    reverse_proxy localhost:7373
}
```

**nginx:**

```nginx
upstream fa_app {
    ip_hash;                          # sticky — required for multi-instance SSE
    server 10.0.0.11:7373;
    server 10.0.0.12:7373;
}

server {
    listen 443 ssl;
    server_name example.com;
    # …ssl_certificate / ssl_certificate_key…

    location / {
        proxy_pass http://fa_app;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Connection "";
        proxy_buffering off;          # CRITICAL for /sse — never buffer the stream
        proxy_read_timeout 24h;       # SSE connections are long-lived
    }
}
```

## Multiple instances

Two changes from single-instance:

1. Same `FA_SIGNING_KEY` everywhere (above).
2. A cross-instance **Broker** so a fragment pushed by instance A reaches
   users connected to instance B. The built-in zero-dependency Redis broker:

   ```go
   b, err := fa.NewRedisBroker(os.Getenv("REDIS_ADDR"))
   if err != nil { log.Fatal(err) }
   app := fa.New(c.Manifest, fa.WithBroker(b), fa.WithSigningKey(key))
   ```

Run the instances behind the sticky LB shown above. Each user's own connection
stays local to one instance; the broker fans events out to the rest.

## Health checks, draining, metrics

Built in, on the app's mux:

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | liveness — process is up |
| `GET /readyz` | readiness — returns **503 once `app.Shutdown()` starts**, so the LB drains the node before it dies |
| `GET /metrics` | **Prometheus exposition format** — the counters below plus `fa_dispatch_duration_seconds` / `fa_fanout_duration_seconds` latency histograms; point a Prometheus scrape job here |
| `GET /debug/metrics` | the same counters as JSON: events in/out, active/total connections, rate-limited, forbidden |

The scaffold already wires SIGTERM → `app.Shutdown()` (drain) →
`http.Server.Shutdown` — rolling deploys work out of the box. Point your
orchestrator's probes at `/healthz` and `/readyz`:

```yaml
# Kubernetes
livenessProbe:  { httpGet: { path: /healthz, port: 7373 } }
readinessProbe: { httpGet: { path: /readyz,  port: 7373 } }
```

Wrap your mux in `fa.LogRequests(mux)` (the scaffold does) for structured
method/path/status/duration request logs.

## Distributed tracing (OpenTelemetry)

FA stays dependency-free by exposing a three-method `fa.Tracer` hook instead of
importing OTel. Implement it with the OTel SDK and pass `fa.WithTracer(t)`;
FA then opens spans around its pipeline — `fa.dispatch` (guard + handler +
emit, per client action), `fa.emit` (sign + publish to the broker), and
`fa.deliver` (a broker message applied to local connections). The emitter's
trace context rides inside the broker message (`Tracer.Inject` /
`Tracer.Extract`, one call each on a W3C `TraceContext` propagator), so a
request stays traceable across instances:

```go
type otelTracer struct{ tr trace.Tracer; prop propagation.TextMapPropagator }

func (t otelTracer) StartSpan(ctx context.Context, name string, attrs map[string]string) (context.Context, func(error)) {
    var kv []attribute.KeyValue
    for k, v := range attrs { kv = append(kv, attribute.String(k, v)) }
    ctx, span := t.tr.Start(ctx, name, trace.WithAttributes(kv...))
    return ctx, func(err error) {
        if err != nil { span.RecordError(err); span.SetStatus(codes.Error, err.Error()) }
        span.End()
    }
}
func (t otelTracer) Inject(ctx context.Context) string {
    c := propagation.MapCarrier{}; t.prop.Inject(ctx, c); return c["traceparent"]
}
func (t otelTracer) Extract(ctx context.Context, carrier string) context.Context {
    return t.prop.Extract(ctx, propagation.MapCarrier{"traceparent": carrier})
}
```

## systemd (plain VM deploy)

```ini
# /etc/systemd/system/myapp.service
[Unit]
Description=myapp (FA)
After=network.target

[Service]
ExecStart=/opt/myapp/myapp
Environment=FA_ADDR=:7373
EnvironmentFile=/etc/myapp/env      # FA_SIGNING_KEY, DATABASE_URL — root-readable only
Restart=always
User=www-data

[Install]
WantedBy=multi-user.target
```

Build with `CGO_ENABLED=0 go build -o myapp .` and copy the binary plus the
`facets/` directory (facets compile at startup from source).

## PaaS (Fly.io, Render, Railway, …)

Anything that runs a Dockerfile runs FA. Checklist:

- set `FA_SIGNING_KEY` as a secret;
- `FA_ADDR=:$PORT` if the platform injects a port;
- health check → `/healthz`;
- if you scale past one instance: enable the platform's session affinity and
  add the Redis broker.

## Production checklist

- [ ] `FA_SIGNING_KEY` set, identical everywhere, stored as a secret
- [ ] TLS terminated at a proxy with **buffering off** for `/sse`
- [ ] Sticky LB + Redis broker if instances > 1
- [ ] LB/orchestrator probes on `/healthz` + `/readyz`
- [ ] `DATABASE_URL` via env; pool sized; queries use request contexts
- [ ] Logs: mux wrapped in `fa.LogRequests`
- [ ] Rotation caveat: changing `FA_SIGNING_KEY` invalidates open pages until
      reload — rotate during a maintenance window (zero-downtime rotation is
      tracked in [ENTERPRISE.md](https://github.com/F33D3R-Inc/fct/blob/main/ENTERPRISE.md))
