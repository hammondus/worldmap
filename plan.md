# Flight map site — build plan

A public site that hosts a world basemap and draws a pilot's flights on it from
a link. The pilot logbook is its first consumer; later projects that need a
basemap use the same archive.

Copy this file into a new directory and build the project there. The last
section, [What the pilot logbook needs](#what-the-pilot-logbook-needs), lists
the changes that belong in `~/dev/pilot-logbook` instead.

Name and module path are yours to pick. This document uses `flightmap` and
`github.com/hammondus/flightmap` as placeholders, and `https://maps.example`
as the deployed origin.

## Why this is a separate project

A multi-user service that receives other people's data does not belong inside a
binary whose first design decision is "local-first with an optional
self-hosted home base". A pilot setting up a home logbook should not be
configuring a public web service.

Splitting it also makes the basemap reusable. The archive is a static file
served over HTTP `Range`; any project that wants a map points at the same URL
and vendors its own MapLibre.

## Decisions already made

These come from the logbook design discussion on 2026-09-10 and are settled.
Read them as constraints, not as options to revisit.

- The site hosts one or more `.pmtiles` archives. Pilots do not download an
  archive to run the logbook. The logbook uses a world archive at zoom 11.
- The logbook's map always pulls tiles from this site. It carries no local
  archive, so there is no 45 MB to distribute and no `make tiles` for pilots.
- **The logbook's map needs the network.** Offline it draws the aerodromes on
  a plain background, plus whatever tiles the browser's HTTP cache still
  holds. This is a deliberate trade against distributing a large file to every
  device. The aerodrome markers and legs keep working offline because their
  coordinates are synced records.
- Sharing goes through a URL fragment by default. A fragment never reaches the
  server, so the default path stores nothing. An opt-in publish endpoint
  handles payloads too long to paste.
- The payload carries coordinates. The site holds no airport database, so it
  draws aerodromes that no airport API has heard of.
- Anyone can extract a regional subset from the public archive themselves.
  `pmtiles extract` reads a remote archive over `Range` and transfers only the
  bytes it needs, so this needs no server-side work and no per-region files.

## Non-goals

- No airport database, no aerodrome lookup, no flight records. The site draws
  what a payload gives it.
- No accounts for viewing. A link is the whole authorisation.
- No tile server, no PostGIS, no rendering. The archive is a static file.
- No serving of a local archive from the logbook binary. If offline basemaps
  become a requirement later, that is a route in the logbook, not a change
  here.

## Architecture

One Go binary serving static bytes, plus one optional stateful endpoint.

Match the logbook's stack so the two repositories read alike: Go 1.26.5,
`github.com/hammondus/nitrokit v0.1.1` for the HTTP toolkit, the web app
embedded with `go:embed`, and a single `main.go` unless it grows past
readable.

### Endpoints

| Route | Purpose |
|---|---|
| `GET /{archive}.pmtiles` | A basemap archive. `Range` and cross-origin. |
| `GET /` | The viewer page. Reads `location.hash` and draws. |
| `GET /vendor/…` | MapLibre, pmtiles.js, sprites, glyphs, layer set. |
| `GET /healthz` | Liveness. |
| `POST /api/publish` | Optional: store a payload, return an unguessable id. |
| `GET /s/{id}` | Optional: the viewer, seeded from a stored payload. |

Serve archives with `http.ServeFile`. It handles `Range`, `ETag`, and
`If-Range` correctly, which is the whole requirement. Do not write a range
handler.

Serve every `.pmtiles` file in the archive directory rather than naming one.
Adding an archive is then a file copy, not a code change.

### The share payload

The logbook composes this, gzips it, base64url-encodes it, and puts it in the
fragment:

```
https://maps.example/#f1.<base64url(gzip(json))>
```

```json
{"v": 1,
 "p": [["YMMB", "Moorabbin", -37.976, 145.102]],
 "l": [[0, 1, 12]],
 "f": [["2019-04-02", [0, 3, 1]]]}
```

- `p` — places: ident, short name, latitude, longitude. Index order matters;
  `l` and `f` refer to places by index.
- `l` — legs: from index, to index, flight count. Direction is ignored, so a
  leg appears once.
- `f` — flights: date and the route as place indexes. Optional.

Measured against the author's logbook of 7,594 flights: 2.7 KB compressed
without the `f` block, 9.1 KB with it. Both fit in a URL for most clients, but
browsers and chat apps disagree on the ceiling, which is what the publish
endpoint is for.

The viewer decodes with `DecompressionStream("gzip")`. Reject an unknown `v`
with a message rather than drawing something wrong.

### The publish store

The only stateful part, and the only part that creates an obligation. Build it
last: everything else works without it.

`POST /api/publish` takes the same JSON, stores it under an unguessable id,
and returns the id and a delete token. Hand the delete link back at publish
time — that is what makes deletion possible without accounts.

Two questions are still open. See [Open questions](#open-questions).

## Tiles

Build archives with the `pmtiles` CLI against a Protomaps daily planet build:

```
pmtiles extract https://build.protomaps.com/<YYYYMMDD>.pmtiles world-z11.pmtiles \
  --maxzoom=11
```

The planet build runs z0-15, read from the header of `20260910.pmtiles` on
2026-09-11. **Zoom 15 is the ceiling** for anything extracted from it. At 512px
tiles, zoom 11 is about 38 m/pixel, where a runway is a visible mark; zoom 14
is about 4.8 m/pixel, where taxiways separate.

### Archives to host

| Archive | Extent | Zoom | Size | Consumer |
|---|---|---|---|---|
| `world-z11.pmtiles` | World | 0-11 | 7.9 GB | The pilot logbook |
| `australia-z14.pmtiles` | Australia | 0-14 | 0.96 GB | `~/dev/aircraft-tracker` |

Name archives by extent and zoom, so the URL says what it is. Add archives as
projects need them; a second file costs disk and no code.

Measured sizes, from dry runs against real builds:

| Extent | Zoom | Size |
|---|---|---|
| World | 0-6 | 45 MB |
| World | 0-7 | 188 MB |
| World | 0-8 | 553 MB |
| World | 0-11 | 7.9 GB |
| World | 0-13 | ~35 GB, extrapolated |
| World | 0-14 | ~70 GB, extrapolated |
| Australia | 0-7 | 4.4 MB |
| Australia | 0-10 | 54 MB |
| Australia | 0-14 | 0.96 GB |
| Australia | 12-14 | 919 MB |

The two extrapolations interpolate between the measured 7.9 GB at zoom 11 and
Protomaps' published 137 GB planet at zoom 15. Treat them as order of
magnitude, not as facts.

To prove the hosting and the cross-origin configuration before moving 7.9 GB,
deploy a zoom 8 world archive first under a throwaway name.

### Higher zoom over Australia, if zoom 11 proves too coarse

Nearly every byte of an archive lives in its top zoom levels: Australia at
z12-14 alone is 919 MB, against 0.96 GB for the whole z0-14 range. So the
`australia-z14.pmtiles` the tracker needs is also, at no extra cost, the
high-zoom archive the logbook would use.

To combine them, give the style **two sources**: `world-z11.pmtiles` with
`maxzoom: 11`, and `australia-z14.pmtiles` with `minzoom: 12`. Generate the
Protomaps layer set twice — its `layers()` function takes the source name as
its first argument — and stop the world layers drawing under the Australian
ones past zoom 11. Getting that wrong renders every label over Australia
twice. That work belongs in the consumer's style generator, not here.

**Do not merge the two archives into one file.** `pmtiles extract --minzoom`
and `pmtiles merge` make it possible, and the result is broken: the merged
header reports max zoom 14, so MapLibre requests z12-14 tiles worldwide. For a
vector archive pmtiles.js returns an empty tile rather than an error when a
tile is missing, and an empty tile draws nothing. Past zoom 11 everywhere
outside the Australian bounding box goes blank instead of overzooming the
zoom 11 tile it already holds. That is worse than having no high-zoom data at
all.

Protomaps purges daily builds after about a week, so pin the date in the
Makefile as a variable and treat bumping it as a deliberate act. Rebuilding is
one transfer of the full archive size, so do not put it in `make release`.

Protomaps documents no rate limit for the daily builds, and their
[downloads page](https://docs.protomaps.com/basemaps/downloads) says:

> Please note that URLs may change and hotlinking to these downloads are
> discouraged. Instead, you should copy the tileset to your own Cloud Storage.

Hosting your own copy is what this project does, so it follows their guidance.
Never point a running service at `build.protomaps.com`.

### Bandwidth

An open archive has no natural ceiling on egress. A map session pulls a few
megabytes. Storage for the archives is cheap; sustained egress is the cost worth
watching. Add a per-IP rate limit on the archive route from the start and log
bytes served, so the number is known before it matters. `Referer` is not a
useful lever: the logbook fetches from its own origin, which varies per pilot.

## Vendored assets

Copy these from `~/dev/pilot-logbook/web/public/vendor/`, which pinned and
checked the versions already. Read that directory's `README.md` first — it
records why the paths carry a version rather than a content hash, and how
`basemap-layers.json` is regenerated.

| Path | Version | Licence |
|---|---|---|
| `maplibre-gl@6.3.0/` | 6.3.0 | BSD-3-Clause |
| `pmtiles@4.5.0/` | 4.5.0 | BSD-3-Clause |
| `basemaps-assets@v4/` | v4 sprites and Noto Sans glyphs | OFL 1.1; BSD-3-Clause |
| `basemap-layers.json` | generated | BSD-3-Clause |

`MapView.svelte` in the logbook is a working reference for assembling the
style. Points worth copying rather than rediscovering:

- `maplibre-gl.css` is not optional. Without it the map gets a canvas and no
  controls or layout, and reports no error.
- pmtiles.js is an IIFE that assigns a `pmtiles` global. `import()` yields
  nothing and `new pmtiles.Protocol()` fails. Load it as a classic script.
- The Protomaps layer set brings its own `background` layer. Adding a second
  one is a duplicate layer id, and MapLibre rejects the whole style.
- Sprites are per flavour. Vendor both `light` and `dark`.
- `renderWorldCopies` defaults to true, which draws markers two or three times
  over at the zoom a world-spanning route frames to.
- The style is not loaded when the constructor returns. Feed GeoJSON sources
  on `load` and `idle` as well as reactively.

Glyphs cover ranges 0-255 and 256-511, which is Basic Latin, Latin-1, and
Latin Extended-A. A label in Cyrillic, Greek, Arabic, or a CJK script 404s and
does not draw. Full Noto coverage costs megabytes. Decide whether a public
site can accept that; the logbook could, because it only shows aerodrome names
the pilot flew to.

## Cross-origin serving

This is the part the logbook depends on. Get it wrong and the logbook's map
fails with no useful error.

The logbook runs on its own origin, usually `http://127.0.0.1:8484` or a LAN
address. Every tile request is cross-origin, and pmtiles.js issues `Range`
requests through `fetch`. The archive route must send:

- `Access-Control-Allow-Origin: *`. The archive is public bytes with no
  credentials, so a wildcard is correct and an origin allowlist is not
  maintainable across every pilot's LAN address.
- `Access-Control-Allow-Headers: Range`, and `Access-Control-Allow-Methods:
  GET, HEAD`, answered on `OPTIONS`.
- `Access-Control-Expose-Headers: Content-Length, Content-Range, ETag`.
  Without `Content-Range` exposed, pmtiles.js cannot read the response it
  just received.
- `Accept-Ranges: bytes`, which `http.ServeFile` sets.
- `Cache-Control: public, max-age=86400`, with the `ETag` that
  `http.ServeFile` already sets. Do not use `immutable` with a long
  `max-age`: the archive name is a setting held by every consumer, so a
  versioned file name forces each of them to edit a setting on every rebuild.
  pmtiles.js handles the archive changing under a live session already — the
  bundle carries an `EtagMismatch` path that invalidates its cached
  directories and retries. Let it do that job.

Verify with a real cross-origin request before calling it done:

```
curl -sD- -o /dev/null -H 'Origin: http://127.0.0.1:8484' \
  -H 'Range: bytes=0-15' https://maps.example/world-z11.pmtiles
```

Expect `206 Partial Content` and every header above.

Behind nginx proxy manager, check that `Range` survives the proxy. Proxy
caching and buffering can turn a range request into a full-body fetch, which
for a 7.9 GB file is a serious failure. Test through the proxy, not just
against the binary.

## Deployment

- Target `linux/arm64`, behind nginx proxy manager, as with the other live
  projects.
- Archives are a volume, never part of the Docker image. A 7.9 GB layer is
  not a build artefact.
- Archives live in a data directory alongside the binary, with the path as a
  flag defaulting to something next to the binary. Follow the logbook's
  `-data` flag as the pattern.
- Start with no archives present and say so plainly. A binary that refuses to
  start because a map file is not there yet is hard to deploy. Serve the
  viewer, return 404 on the archive route, and log the directory it scanned.

## Repository layout

Follow the conventions in `~/.claude/CLAUDE.md`:

- `DESIGN-DECISIONS.md` — every choice made for a reason, including the ones
  carried over from this plan. This document is a plan, not a record; the
  record belongs in the new repository.
- `Makefile` with the canonical targets: `build`, `test`, `run`, `release`,
  `clean`, `docker-build`, `deploy`, `logs`. Add `tiles` for the extract, with
  the pinned build date and max zoom as variables. Mark every target `.PHONY`.
- `README.md` — what the site is, how to build the archive, how to point a
  logbook at it.
- Default branch `master`.

## Phases

1. **Serve the archive.** The binary, the archive route, CORS, `Range`, the
   Makefile, a zoom 8 world archive. Nothing else. The logbook can use the
   site at the end of this phase.
2. **The viewer.** Vendored assets, the style, the fragment format, decoding,
   markers, and legs. Reject unknown payload versions.
3. **Zoom 11 and deployment.** The full extract, the volume, the proxy, the
   rate limit, and the cross-origin check through the proxy.
4. **The publish store.** Only after the open questions below are answered.

## Open questions

Both were raised in the original design discussion and never answered. Neither
blocks phases 1 to 3.

- **Retention.** A published payload that includes the `f` block is a movement
  history with dates. Does it expire on its own — 30 days, a year, never — and
  does deleting it need anything beyond holding the delete link?
- **Identity.** An unguessable id and no account is the smallest design that
  works, and it means an abuse report has nobody to act against. An account
  brings back everything the fragment design avoids. The recommendation is no
  accounts, a size cap, a per-IP rate limit, and a delete link handed back at
  publish time.

## What `~/dev/aircraft-tracker` needs

The tracker is a second consumer, not a dependency of this project. Treat
moving it as a later decision, once the site has proven uptime.

It already works: `make tiles` builds `tiles/australia.pmtiles` at zoom 14,
and `web.go:227` serves it from its own origin behind `requireAuth`, with a
stat-derived token in the path as a cache-buster.

- **What it gains:** `make tiles` leaves the repository, and the deploy stops
  carrying a 0.96 GB volume.
- **What it costs:** a live operational display gains a network dependency on
  another host. The tiles themselves are not secret — the archive is a stock
  OpenStreetMap basemap — but a map that goes blank because a different server
  is unreachable is a worse trade for the tracker than for a logbook read at a
  desk.

If it does move, `web/static/app.js:142` caps the map at `maxZoom: 14`, so the
site must host an Australian archive that reaches zoom 14. Hosting zoom 11
only would silently degrade the tracker: MapLibre overzooms past an archive's
max zoom, so the map stays readable but stops gaining detail exactly where an
aircraft in the circuit needs it.

## What the pilot logbook needs

Changes in `~/dev/pilot-logbook`, not in the new project.

### To get a basemap

Nothing in the code. In the app, open **Settings** and set the map tiles field
to `https://maps.example/world-z11.pmtiles`.

The rest already works:

- `MapView.svelte` builds a `pmtiles://` source from that setting and falls
  back to a plain background when it is empty.
- `mapCSP` in `main.go:353` reads the setting from the store per request and
  adds its origin to `connect-src`, so the policy follows the setting with no
  restart.
- `sw.js:55` returns early for cross-origin requests, so tiles bypass the
  service worker and use the ordinary HTTP cache.
- MapLibre, pmtiles.js, sprites, glyphs, and the layer set are vendored under
  `web/public/vendor/`. Only the archive bytes cross the network.

### To share a map

1. In `web/src/lib/model.ts`, add `map_site` to `Settings`, alongside
   `map_tiles`. Deriving the site origin from the archive URL is fragile; a
   pilot can host the archive on object storage and the viewer elsewhere.
2. In `web/src/Settings.svelte`, add the field.
3. In `web/src/MapView.svelte`, add a share composer with a scope choice: this
   flight, a date range, or everything. Build the payload from
   `book.mapPoints()` and `book.mapLegs()`, which already produce the shapes
   the payload needs — `MapPoint` carries `ident`, `place`, and `visits`;
   `MapLeg` carries `from`, `to`, and `flights`. Drop points whose `place` is
   undefined, as the map already does.
4. Compress with `CompressionStream("gzip")` and base64url-encode. The
   encoding helpers in `web/src/lib/sync.svelte.ts` are the pattern for
   base64url, but they take a string; the payload is bytes.
5. Opening the link is a navigation, so it needs no Content-Security-Policy
   change. Calling `POST /api/publish` does: `originOf` must produce the site
   origin, which means `mapCSP` needs to read `map_site` as well as
   `map_tiles`.

### Documentation to correct

`README.md:109-138` currently tells the pilot to build their own archive with
`pmtiles extract` and host it somewhere with CORS. That is no longer the
route. Replace it with the site URL and one sentence saying the map needs the
network. Keep the extract instructions as an aside for a pilot who wants a
regional archive of their own, and point them at the public archive as the
source rather than `build.protomaps.com`.

`DESIGN-DECISIONS.md:736` describes the archive URL as "a service the pilot
chooses". That is still true, but the reason it is a setting, and the fact
that the map needs the network, are worth stating alongside it.
