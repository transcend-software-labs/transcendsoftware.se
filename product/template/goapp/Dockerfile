# Single-binary app image. Pure-Go SQLite → CGO_ENABLED=0, no build deps.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app-bin .

FROM alpine:3.20
RUN adduser -D app && mkdir -p /data && chown app /data
USER app
ENV DATA_DIR=/data ADDR=:8080
COPY --from=build /app-bin /usr/local/bin/app
EXPOSE 8080
CMD ["app"]
