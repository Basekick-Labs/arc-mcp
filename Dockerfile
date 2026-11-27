# arc-mcp Docker image.
#
# goreleaser builds the binary on the host and copies it into this image — we do
# NOT rebuild from source at docker-build time. That's why there's no builder
# stage: it would be wasted work.
#
# The base is distroless static — no shell, no package manager, no libc.
# arc-mcp is pure Go (CGO_ENABLED=0), so this is the smallest and safest
# viable base. Runs as nonroot (uid 65532) by default.
#
# The default is stdio. For remote clients run it with
#   --transport http --listen 0.0.0.0:8080
# and an inbound token (ARC_MCP_HTTP_TOKEN_FILE or ARC_MCP_HTTP_TOKEN): MCP is
# then served over Streamable HTTP at /mcp, with /healthz for liveness.

FROM gcr.io/distroless/static-debian12:nonroot

COPY arc-mcp /usr/local/bin/arc-mcp

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/arc-mcp"]
