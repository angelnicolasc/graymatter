FROM alpine:3.20
ARG TARGETARCH
RUN apk add --no-cache curl tar ca-certificates && \
    curl -sSL https://github.com/angelnicolasc/graymatter/releases/download/v0.15.0/graymatter_0.15.0_linux_${TARGETARCH}.tar.gz \
    | tar -xz -C /usr/local/bin graymatter && \
    chmod +x /usr/local/bin/graymatter
ENTRYPOINT ["graymatter", "mcp", "serve"]
