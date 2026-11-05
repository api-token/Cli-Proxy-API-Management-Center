# Build context: this directory (plugins/clickhouse-sink/deriver).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/deriver .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/deriver /deriver
ENTRYPOINT ["/deriver"]
