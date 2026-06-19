# syntax=docker/dockerfile:1
# The Facet toolchain image: one static, dependency-free binary. Build it once,
# then run any app with `docker run … facet run /app/app.fct`. The client runtime
# (facet.js) is embedded in the binary, so the image needs nothing else.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/facet ./cmd/facet

# distroless static: no shell, no package manager — minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/facet /usr/local/bin/facet
# Behind TLS, only send the session cookie over HTTPS.
ENV FACET_SECURE_COOKIES=1
EXPOSE 7373
ENTRYPOINT ["facet"]
CMD ["version"]
