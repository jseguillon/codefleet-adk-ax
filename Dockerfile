# Both AX runner and worker commands are prebuilt: no model or package bootstrap.
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/codefleet-worker ./cmd/codefleet-worker \
 && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/ax-task-runner ./cmd/ax-task-runner \
 && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/codefleet ./cmd/codefleet
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git python3 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
