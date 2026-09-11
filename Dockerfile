# One build stage and one runtime stage. Pure Go with no CGo, so the binary
# is static and runs in a distroless base with no libc and no shell. The
# viewer is plain HTML embedded with go:embed, so there is no node stage.
#
# The build runs on the deploy host under `docker compose build`, so
# `go build` produces the host's architecture without a cross-compile.

FROM golang:1.27 AS build
WORKDIR /src
# Module files first, so dependency downloads cache as their own layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worldmap .

FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /app
COPY --from=build /out/worldmap /app/worldmap
EXPOSE 8080
# The binary probes itself: distroless has no shell and no curl.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/app/worldmap", "-healthcheck", "-addr", ":8080"]
ENTRYPOINT ["/app/worldmap"]
# -data is required here: the flag default puts the directory beside the
# executable, which is the image's read-only layer, not the mount.
CMD ["-addr", ":8080", "-data", "/data"]
