# syntax=docker/dockerfile:1
# Vigie in a container: the static binary on a distroless base, as a
# non-root user. The binary is cross-compiled for the target platform, so a
# multi-platform build needs no emulation.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/OlivMartins/vigie/internal/version.version=${VERSION} -X github.com/OlivMartins/vigie/internal/version.commit=${COMMIT}" \
      -o /out/vigie ./cmd/vigie

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vigie /usr/local/bin/vigie
LABEL org.opencontainers.image.title="Vigie" \
      org.opencontainers.image.description="Every tool call of your AI agents, seen, pinned, filtered: an MCP proxy and gateway with OpenTelemetry tracing." \
      org.opencontainers.image.source="https://github.com/OlivMartins/vigie" \
      org.opencontainers.image.licenses="Apache-2.0"
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/vigie"]
# Listening on every interface of the container: the configuration must give
# gateway.tokens, or gateway.allow_anonymous when a proxy authenticates.
CMD ["gateway", "--config", "/etc/vigie/vigie.yaml", "--listen", "0.0.0.0:8787"]
