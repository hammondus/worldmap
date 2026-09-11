# Design decisions

Why this code is shaped the way it is. `plan.md` is the plan; this file is the
record, including the decisions the plan settled before any code existed.

## Carried over from the plan

These came out of the pilot-logbook design discussion on 2026-09-10 and were
settled before this repository existed. `plan.md` holds the reasoning in full.

- **A separate project, not a route in pilot-logbook.** A multi-user service
  that receives other people's data does not belong in a binary whose first
  design decision is "local-first with an optional self-hosted home base".
  Splitting it also makes the basemap reusable: any project that wants a map
  points at the same URL and vendors its own MapLibre.
- **The logbook's map needs the network.** Pilots do not download an archive.
  This trades an offline basemap against distributing a 45 MB file to every
  device. Aerodrome markers and legs keep working offline, because their
  coordinates are synced records rather than tiles.
- **Sharing goes through a URL fragment.** A fragment never reaches the server,
  so the default path stores nothing. The publish endpoint, for payloads too
  long to paste, is phase 4 and opt-in.
- **No airport database.** The share payload carries coordinates, so the site
  draws aerodromes that no airport API has heard of.
- **One archive per extent and zoom, never merged.** `pmtiles merge` would
  produce a header advertising the highest zoom worldwide, so MapLibre would
  request z12-14 tiles outside the Australian bounding box. pmtiles.js returns
  an empty tile rather than an error for a missing vector tile, and an empty
  tile draws nothing, so the map would go blank past zoom 11 instead of
  overzooming the zoom 11 tile it already holds.

## Decisions made while building phase 1

### The viewer is plain HTML, not Svelte

The plan says to match the logbook's stack. The logbook's stack includes Svelte
and Vite, because the logbook is an offline-first application with synced state.
The viewer decodes a fragment and draws a map. Plain HTML with a module script
keeps the Docker build to one stage, keeps `make` to the canonical targets with
no `web` target, and removes a second toolchain from a project whose job is
serving static bytes.

### nitrokit v0.3.0 and Go 1.27, not the logbook's pinned versions

The plan pins `nitrokit v0.1.1` and Go 1.26.5 to match the logbook. This
repository is new and has no users, so there is nothing to stay compatible
with. Starting a new project a version behind buys nothing.

### The egress limit charges bytes, not requests

The plan says to add a per-IP rate limit on the archive route. The unit it does
not name is the one that matters.

One map session issues hundreds of small `Range` requests: pmtiles.js reads the
header, then a directory, then a tile per visible cell, and does it again on
every pan and zoom. A requests-per-second ceiling loose enough to let a real
session through does nothing to bound a client pulling the whole 7.9 GB
archive, which is the case worth bounding. Bytes are also the cost being
protected.

The first implementation was a local token bucket whose token was a byte. It
duplicated the shape of `nitrokit.Limiter` closely enough to be the exact
problem that module exists to stop, so the primitive moved into nitrokit as
`Limiter.Charge` (v0.4.0) and the local copy was deleted.

The handler admits a request with `Allow`, serves it while `countingWriter`
totals the body bytes, then calls `Charge` with the total. The bucket is
allowed to go negative, so one oversized response overdraws the address and the
next request waits, rather than a response being cut off partway through once
its true cost is known. `Allow` spends one token as an admission toll, which
against a 256 MiB burst is not worth avoiding.

The defaults are 128 KiB/s sustained with a 256 MiB burst, both flags. A
session costs a few megabytes, so the burst is roughly fifty sessions at full
speed before the rate starts to bite.

### `X-Forwarded-For` is trusted from private addresses by default

Every deployment of this sits behind Nginx Proxy Manager on a container
network. Trusting no peer there would attribute every request to the proxy's
address and collapse the per-address rate limit into one global bucket, which
is worse than no limit at all, because the first busy client would lock out
everyone. `make run` passes `-trusted-proxies=none`, because nothing stands in
front of the binary locally and believing the header from a direct caller would
let anyone pick which bucket to spend.

### Cross-origin serving and the ETag come from nitrokit

The plan says `http.ServeFile` sets an `ETag`. It does not. `ServeContent` and
`ServeFile` set `Last-Modified` and answer `Range`, `If-Range`, and
`If-None-Match`, but they generate no validator. Without one, a client
revalidating after `max-age` has only a timestamp, and pmtiles.js has nothing
to drive its `EtagMismatch` path when an archive changes mid-session.

Both halves of this were written locally first and then moved into nitrokit
v0.4.0, because the workspace already had other copies of each:

- `nitrokit.ServeFileRange` sets `FileETag` — a strong validator built from
  file size and modification time — before calling `ServeContent`, which is
  the ordering the precondition check depends on. `aircraft-tracker` had
  written the same validator, to the same format string, without either
  project knowing about the other.
- `nitrokit.CORS` describes the policy and answers the preflight.
  `archiveCORS` is a wildcard origin with `Range` allowed and
  `Content-Length, Content-Range, ETag, Accept-Ranges` exposed.

A wildcard origin is correct here: the archive is public bytes with no
credentials, and an allowlist would have to name every pilot's LAN address.
The policy wraps the route rather than sitting inside the handler, so its
headers ride every reply including a 404 and a 429 — a refusal a browser
cannot read is reported to the page as an opaque network error.

One consequence of the middleware form: an `OPTIONS` that carries no
`Access-Control-Request-Method` is not a preflight, so it reaches the handler.
The handler answers 405 rather than serving gigabytes.

### The archive is opened per request, through `os.Root`

Opening the data directory per request rather than holding it from startup
means an archive copied in after the process started is served without a
restart, which is what makes "publishing an archive is a file copy" true. The
cost is one `openat` per request, which is nothing next to serving megabytes.

`os.Root` confines the open to the data directory. The router's wildcard is a
single path segment and the handler requires a `.pmtiles` suffix, so a
traversing name cannot reach the handler in the first place; `os.Root` makes
the confinement a property of the code rather than of that argument.

### The archive routes carry no access log

`nitrokit.AccessLog` wraps the index page only. One map session would otherwise
write hundreds of lines and bury everything else. `traffic` accumulates
requests, bytes, and refusals per archive, and logs a rollup every 15 minutes
and once at shutdown. That answers the question the plan actually asks — how
much is leaving the host — without the volume.

### `WriteTimeout` is zero, with `WriteBudget` in its place

`nitrokit.NewServer` sets a 30-second `WriteTimeout`, which suits a page-serving
app and would cut an archive download at 30 seconds. The server sets it to zero
and wraps the handler in `nitrokit.WriteBudget`, which renews the write deadline
as writes make progress. A response may then take as long as it takes, but a
single write that stalls for 30 seconds still kills the connection.

### `Cache-Control: public, max-age=86400`, deliberately not `immutable`

The archive name is a setting held by every consumer. A versioned file name
would force each of them to edit a setting on every rebuild, so a rebuild reuses
the name, and `immutable` would tell a browser it never has to ask again. The
`ETag` carries the change instead.

### The archive directory is excluded from the build context

`.dockerignore` exists for one line: `data/`. Docker does not read
`.gitignore`, and the runtime bind mount has no bearing on the build, so
`COPY . .` sent the whole 8.5 GB archive directory to the daemon on every
build. The context is 658 bytes with the exclusion.

The rest of the file is small: build output, `go.work` — kept out
deliberately, so a container build resolves nitrokit from the module proxy
exactly as the deploy host does — and `.git`. Documentation is not excluded,
even though it is not needed to compile: the saving is 30 KB, and a future
`go:embed` of a Markdown file would fail in a way that takes a while to
explain.

### Archives are a bind mount, not a named volume

The plan says archives are a volume and never part of the image. A named Docker
volume would mean copying an 8 GB file through a throwaway container. A bind
mount is a directory you can `scp` into. The mount is read-only: the server
never writes an archive.

### The stylesheet is served unhashed, with a one-hour `max-age`

`nitrokit.Assets` fingerprints an embedded tree and serves it with an immutable
year-long cache. Phase 1 has one stylesheet, so the fingerprinting machinery
would cost template plumbing for no benefit. When phase 2 vendors MapLibre,
pmtiles.js, sprites, and glyphs, switch to `nitrokit.Assets` and take the
immutable caching for the whole tree.

### Glyphs will cover Latin only

Decided for phase 2, recorded here because it is a product decision rather than
a coding one. The vendored glyphs cover ranges 0-255 and 256-511: Basic Latin,
Latin-1, and Latin Extended-A. A label in Cyrillic, Greek, Arabic, or a CJK
script 404s and does not draw. Full Noto coverage costs megabytes. Glyphs are
per-range files, so widening coverage later is a copy and a rebuild of nothing.

## Open questions

Neither blocks phases 1 to 3. Both need answering before the publish store in
phase 4.

- **Retention.** A published payload that includes the `f` block is a movement
  history with dates. Does it expire on its own, and does deleting it need
  anything beyond holding the delete link?
- **Identity.** An unguessable id and no account is the smallest design that
  works, and it means an abuse report has nobody to act against. The
  recommendation is no accounts, a size cap, a per-address rate limit, and a
  delete link handed back at publish time.
