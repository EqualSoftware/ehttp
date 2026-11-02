# The example server as a small Alpine image, run as a non-root user.
# Build from the repository root (see compose.yaml).

# build: the example server binary.
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /example ./examples/server

# example: the server, as a non-root user. main reads ./examples/server.
FROM alpine:3.24.3 AS example
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /example /app/example
COPY examples/server/templates /app/examples/server/templates
COPY examples/server/static /app/examples/server/static
USER app
EXPOSE 3000
ENTRYPOINT ["/app/example"]
