FROM alpine:3.20
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/angelnicolasc/graymatter" \
      org.opencontainers.image.description="Persistent agent memory as a single Go binary: knowledge graph, TUI observability, zero infra." \
      org.opencontainers.image.licenses="MIT" \
      io.modelcontextprotocol.server.name="io.github.angelnicolasc/graymatter"
RUN apk add --no-cache curl tar ca-certificates && \
    curl -fsSL https://github.com/angelnicolasc/graymatter/releases/download/v0.21.0/graymatter_0.21.0_linux_${TARGETARCH}.tar.gz -o /tmp/graymatter.tar.gz && \
    tar -xz -f /tmp/graymatter.tar.gz -C /usr/local/bin graymatter && \
    rm /tmp/graymatter.tar.gz && \
    chmod +x /usr/local/bin/graymatter
ENTRYPOINT ["graymatter", "mcp", "serve"]
