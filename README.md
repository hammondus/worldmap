# worldmap

A public site that hosts basemap archives and draws a pilot's flights on one
from a link.

The site serves `.pmtiles` archives as static bytes over HTTP `Range`, with
cross-origin headers that let a consumer on any origin read them. The first
consumer is [pilot-logbook](https://github.com/hammondus/pilot-logbook), which
carries no archive of its own and pulls tiles from here.

Live at `https://worldmap.hammond.zone`.

## Status

Phase 1 of [plan.md](plan.md): the binary, the archive route, cross-origin
serving, `Range`, and the egress limit. The viewer that draws a flight map from
a URL fragment is phase 2 and is not built yet. The index page lists what is
published.

The egress limit needs `Limiter.Charge`, which lands in nitrokit v0.4.0. Until
that tag exists, the build resolves nitrokit through a `go.work` beside this
file, and `docker compose build` fails because `go.work` is outside the build
context. To build the container, tag nitrokit v0.4.0, run `go get
github.com/hammondus/nitrokit@v0.4.0`, and delete `go.work`.

## Endpoints

| Route | Purpose |
|---|---|
| `GET /{archive}.pmtiles` | A basemap archive. `Range` and cross-origin. |
| `GET /` | The index: what is published, and how to point a client at it. |
| `GET /healthz` | Liveness. |

## Pointing a client at an archive

In a MapLibre style, prefix the archive URL with `pmtiles://`:

```
pmtiles://https://worldmap.hammond.zone/world-z11.pmtiles
```

In pilot-logbook, open **Settings** and set the map tiles field to
`https://worldmap.hammond.zone/world-z11.pmtiles`. The logbook's map needs the
network from then on: offline it draws aerodromes on a plain background, plus
whatever tiles the browser's HTTP cache still holds.

## Building an archive

Archives come from a [Protomaps](https://protomaps.com) daily planet build.
`pmtiles extract` reads the source over HTTP `Range` and transfers only the
bytes it needs.

To build the world archive, run:

```
make tiles
```

The output lands in `data/world-z11.pmtiles` at about 7.9 GB. `make tiles
WORLD_ZOOM=8` builds a 553 MB archive instead, which is enough to prove the
hosting before you move the full one.

Protomaps purges daily builds after about a week. The date is pinned as
`PLANET_DATE` in the `Makefile`; a stale date fails with a 404. Each bump
costs one full transfer of the archive, so `tiles` is not part of `build`,
`release`, or `deploy`.

Never point a running service at `build.protomaps.com`. Protomaps discourages
hotlinking and asks you to copy the tileset to your own storage, which is what
this site is.

### Extracting a region

`pmtiles extract` works against the published archive too, so a regional subset
needs no server-side work:

```
pmtiles extract https://worldmap.hammond.zone/world-z11.pmtiles melbourne.pmtiles \
  --bbox=144.5,-38.5,145.6,-37.4
```

## Running it

```
make build
./worldmap -data ./data -trusted-proxies=none
```

| Flag | Default | Purpose |
|---|---|---|
| `-addr` | `:8080` | Listen address. |
| `-data` | `data` beside the executable | Directory of `.pmtiles` archives. |
| `-rate` | `131072` | Sustained archive bytes per second per client address. `0` disables the limit. |
| `-burst` | `268435456` | Archive bytes a client address may take at full speed before `-rate` applies. |
| `-trusted-proxies` | `private` | Whose `X-Forwarded-For` to believe: `private`, `none`, or a comma-separated list of CIDR blocks. |
| `-report` | `15m` | How often to log an egress rollup. `0` disables it. |
| `-healthcheck` | off | Probe a running server's `/healthz` and exit. |

Publishing an archive is a file copy. The server lists the data directory on
every request, so a file dropped in is served without a restart. A missing data
directory is not a startup failure: the server logs what it scanned, serves the
index, and returns 404 on the archive route.

## Deploying

The site runs as a container behind Nginx Proxy Manager on
`craigus.hammond.zone`, on the shared `blobbyboo` network, with no published
ports.

Archives are a bind mount, never part of the image. To publish one, copy it to
the host and restart nothing:

```
scp data/world-z11.pmtiles craigus.hammond.zone:/srv/worldmap/data/
```

On the host, run:

```
make deploy
```

In Nginx Proxy Manager, leave caching off for this host. A proxy cache can
collapse a `Range` request into a full-body fetch, which for an 8 GB archive
means the proxy downloads the whole file to answer a 16-byte read.

### Verifying cross-origin serving

The logbook depends on this. To check it through the proxy rather than against
the container, run:

```
make check-cors URL=https://worldmap.hammond.zone/world-z11.pmtiles
```

Expect `206 Partial Content`, a `Content-Range` header, and
`Access-Control-Expose-Headers` listing `Content-Range`. Without `Content-Range`
exposed, pmtiles.js cannot read the response it just received, and the browser
reports an opaque network error.

## Licences

The server is this repository's code. The map data comes from
[OpenStreetMap](https://openstreetmap.org/copyright) contributors under the
Open Database License, and the tiles are built with
[Protomaps](https://protomaps.com) under BSD-3-Clause. Any page that renders
these tiles must credit OpenStreetMap.
