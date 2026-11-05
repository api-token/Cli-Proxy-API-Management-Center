# Build context: plugins/clickhouse-sink (the deriver imports ../redact).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY deriver/go.mod deriver/go.sum ./deriver/
RUN cd deriver && go mod download
COPY . .
RUN cd deriver && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/deriver .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/deriver /deriver
ENTRYPOINT ["/deriver"]
