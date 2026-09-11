# worldmap: one Go binary serving .pmtiles archives and a viewer.
# Deploys as a container behind Nginx Proxy Manager, which terminates TLS;
# the container serves plain HTTP on 8080. Archives live in a bind-mounted
# directory, never in the image.

BINARY := worldmap

GOFLAGS := -trimpath
LDFLAGS := -s -w

# The Protomaps daily planet build to extract from. Protomaps purges these
# after about a week, so a stale date fails with a 404 rather than serving
# something older. Bumping it is a deliberate act: each bump means
# re-transferring a full archive.
PLANET_DATE := 20260910
PLANET := https://build.protomaps.com/$(PLANET_DATE).pmtiles

# Zoom 11 is about 38 m/pixel at 512 px tiles, where a runway is a visible
# mark. Zoom 15 is the ceiling of the planet build, so nothing extracted
# from it can go higher. Override on the command line for a smaller proof
# archive: make tiles WORLD_ZOOM=8
WORLD_ZOOM := 11
AU_ZOOM := 14
AU_BBOX := 112.9,-43.7,153.7,-9.1

DATA := data

.PHONY: build test run release clean docker-build deploy logs tiles tiles-australia check-cors

## build: build the server for this machine
build:
	go build $(GOFLAGS) -o $(BINARY) .

## test: vet + tests
test:
	go vet ./...
	go test ./...

## run: build, then serve the archives in ./data on port 8080
# -trusted-proxies=none because nothing is in front of the binary here:
# believing X-Forwarded-For from a direct caller would let anyone pick
# which rate-limit bucket to spend.
run: build
	./$(BINARY) -addr :8080 -data $(DATA) -trusted-proxies=none

## release: a stripped static binary for the deploy target
# linux/arm64 only. This is a server, not a tool anyone runs locally; the
# dev machine uses `make build`.
release:
	rm -rf dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	cd dist && shasum -a 256 $(BINARY)-* > SHA256SUMS
	@ls -la dist

## clean: remove build output, never the archives
clean:
	rm -rf $(BINARY) dist

## docker-build: check the image builds
docker-build:
	docker compose build

## deploy: on the server, pull and restart the container
deploy:
	git pull
	docker compose up -d --build

## logs: follow the container's logs
logs:
	docker compose logs -f worldmap

## tiles: extract the world archive from the pinned planet build
# One transfer of the full output size, so this is not part of release and
# not part of deploy. `pmtiles extract` reads the source over HTTP Range
# and takes only the bytes it needs.
tiles:
	@mkdir -p $(DATA)
	pmtiles extract $(PLANET) $(DATA)/world-z$(WORLD_ZOOM).pmtiles --maxzoom=$(WORLD_ZOOM)

## tiles-australia: extract the Australian archive at zoom 14
# Kept as a separate file, never merged into the world archive: a merged
# header would advertise zoom 14 worldwide, and MapLibre would then ask
# for tiles that do not exist everywhere outside the bounding box.
tiles-australia:
	@mkdir -p $(DATA)
	pmtiles extract $(PLANET) $(DATA)/australia-z$(AU_ZOOM).pmtiles --maxzoom=$(AU_ZOOM) --bbox=$(AU_BBOX)

## check-cors: prove a cross-origin range request works against a URL
# Run this through the proxy, not only against the binary: proxy caching
# or buffering can turn a range request into a full-body fetch, which for
# a multi-gigabyte archive is a serious failure. Expects 206 Partial
# Content and the full Access-Control-Expose-Headers set.
#   make check-cors URL=https://worldmap.hammond.zone/world-z11.pmtiles
URL ?= http://127.0.0.1:8080/world-z11.pmtiles
check-cors:
	curl -sD- -o /dev/null -H 'Origin: http://127.0.0.1:8484' -H 'Range: bytes=0-15' '$(URL)'
	@echo '--- preflight ---'
	curl -sD- -o /dev/null -X OPTIONS -H 'Origin: http://127.0.0.1:8484' \
	  -H 'Access-Control-Request-Method: GET' \
	  -H 'Access-Control-Request-Headers: range' '$(URL)'
