# Build a static binary, then ship it on a minimal base.
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /app .

FROM gcr.io/distroless/static-debian12
WORKDIR /
COPY --from=build /app /app
COPY facets /facets
# Required in production (see README "Running in production"):
#   FA_SIGNING_KEY  — same value on every instance (openssl rand -hex 32)
# Run multiple instances behind a STICKY load balancer.
ENV FA_ADDR=:7373
EXPOSE 7373
# Health: GET /healthz (liveness)  •  GET /readyz (readiness; 503 while draining)
ENTRYPOINT ["/app"]
