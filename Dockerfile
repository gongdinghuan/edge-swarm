ARG GO_IMAGE=golang:1.27.3-bookworm
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/control ./cmd/control && CGO_ENABLED=0 go build -trimpath -o /out/agent ./cmd/agent

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* && useradd -m -u 10001 swarm
COPY --from=build /out/ /usr/local/bin/
COPY deploy/control/control-entrypoint.sh /usr/local/bin/control-entrypoint
RUN sed -i 's/\r$//' /usr/local/bin/control-entrypoint && chmod 0755 /usr/local/bin/control-entrypoint
USER swarm
WORKDIR /home/swarm
ENTRYPOINT ["control"]
