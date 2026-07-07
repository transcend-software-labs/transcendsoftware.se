# Single-binary app image. Pure-Go SQLite → CGO_ENABLED=0, no build deps.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app-bin .

FROM alpine:3.20
RUN adduser -D app && mkdir -p /data && chown app /data
# Litestream streams the SQLite WAL to object storage for continuous backup +
# restore-on-boot. No-op unless the Forge orchestrator sets LITESTREAM_* (see
# entrypoint.sh / litestream.yml).
COPY --from=litestream/litestream:0.3.13 /usr/local/bin/litestream /usr/local/bin/litestream
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
USER app
ENV DATA_DIR=/data ADDR=:8080
COPY --from=build /app-bin /usr/local/bin/app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
