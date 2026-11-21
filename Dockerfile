# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=1
COPY go.mod go.sum ./
# The template builds against its local kit/ (see the replace in go.mod). A
# project created with make init has no kit/ and downloads the published
# module instead; the wildcard copies nothing then.
COPY ki[t] ./kit
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN go build -trimpath -o /out/api ./cmd/api \
    && go build -trimpath -o /out/migrate ./cmd/migrate

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/data \
    && chown 10001:10001 /app/data
WORKDIR /app
COPY --from=build --chown=10001:10001 /out/api /out/migrate ./
ENV HTTP_ADDR=:8080 \
    STORAGE=sqlite \
    DATABASE_URL=/app/data/app.db \
    GIN_MODE=release \
    LOG_OUTPUTS=stdout
USER 10001:10001
EXPOSE 8080
CMD ["/app/api"]
