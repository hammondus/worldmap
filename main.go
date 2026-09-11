// Command worldmap serves basemap archives over HTTP, and a viewer that
// draws a flight map from a payload carried in the URL fragment.
//
// An archive is a .pmtiles file in the data directory, served as static
// bytes: Range requests, an ETag, and permissive cross-origin headers, so
// a consumer on any origin can read it with pmtiles.js. Adding an archive
// is a file copy, not a code change.
package main

import (
	"cmp"
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hammondus/nitrokit"
)

//go:embed web
var embedded embed.FS

const archiveExt = ".pmtiles"

// archiveMaxAge is a day, deliberately without `immutable`. The archive
// name is a setting held by every consumer, so a rebuild reuses the name
// rather than taking a new one, and `immutable` would tell a browser it
// never has to ask again. The ETag carries the change instead: pmtiles.js
// already has an EtagMismatch path that drops its cached directories and
// retries when the archive moves under a live session.
const archiveMaxAge = 24 * time.Hour

// assetMaxAge is the hour that an unhashed asset URL gets: /style.css can
// change under a client, so it cannot be cached for a year.
const assetMaxAge = time.Hour

// Egress budget per client address, in bytes. The limit is on bytes, not
// on requests, because bytes are the cost: one map session fires a burst
// of small Range requests, so any requests-per-second ceiling loose
// enough for a real session is far too loose to bound what leaves the
// host. A session costs a few megabytes, so the burst is roughly fifty
// sessions at full speed before the sustained rate starts to bite.
const (
	defaultRate  = 128 << 10 // 128 KiB/s sustained
	defaultBurst = 256 << 20 // 256 MiB at full speed
)

// writeBudget bounds a single write that makes no progress, standing in
// for the server-wide WriteTimeout that a multi-gigabyte download cannot
// have. Same semantics as nginx's send_timeout.
const writeBudget = 30 * time.Second

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "", "directory of .pmtiles archives (default: data beside the executable)")
	rate := flag.Float64("rate", defaultRate, "sustained archive bytes per second per client address; 0 disables the limit")
	burst := flag.Float64("burst", defaultBurst, "archive bytes a client address may take at full speed before -rate applies")
	trusted := flag.String("trusted-proxies", "private", `whose X-Forwarded-For to believe: "private", "none", or a comma-separated CIDR list`)
	every := flag.Duration("report", 15*time.Minute, "how often to log an egress rollup; 0 disables it")
	healthcheck := flag.Bool("healthcheck", false, "probe a running server's /healthz and exit; for the container HEALTHCHECK")
	flag.Parse()

	if *healthcheck {
		if err := nitrokit.HealthProbe(*addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dir, err := archiveDir(*data)
	if err != nil {
		log.Error("resolve data directory", "err", err)
		os.Exit(1)
	}
	trust, err := proxyTrust(*trusted)
	if err != nil {
		log.Error("parse -trusted-proxies", "value", *trusted, "err", err)
		os.Exit(1)
	}

	app, err := newServer(dir, trust, *rate, *burst, log)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	app.announce()

	// The rollup goroutine gets its own cancellation: nitrokit.Run owns
	// the signal handler, so the only reliable moment to write the final
	// numbers is after it returns.
	ctx, cancel := context.WithCancel(context.Background())
	if *every > 0 {
		go app.traffic.reportEvery(ctx, *every, log)
	}

	handler := nitrokit.WriteBudget(writeBudget, nitrokit.SecureHeaders("", "", app.routes()))
	srv := nitrokit.NewServer(*addr, handler)
	// An archive is gigabytes. A server-wide write timeout would cut the
	// download at whatever it is set to, so it goes to zero and
	// WriteBudget takes over slow-client protection per write.
	srv.WriteTimeout = 0

	log.Info("listening", "addr", *addr, "data", dir)
	runErr := nitrokit.Run(ctx, srv)
	cancel()
	app.traffic.report(log)
	if runErr != nil {
		log.Error("serve", "err", runErr)
		os.Exit(1)
	}
}

// archiveDir resolves the -data flag. An empty flag means a directory
// beside the executable, which is what a pilot running the binary from a
// download expects; the container passes the volume path explicitly.
func archiveDir(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "data"), nil
}

// proxyTrust turns the -trusted-proxies flag into a trust list. The
// default is the private ranges, because every deployment of this sits
// behind nginx proxy manager on a container network: trusting nobody
// there would attribute every request to the proxy's address and collapse
// the per-address rate limit into one global bucket.
func proxyTrust(spec string) (*nitrokit.ProxyTrust, error) {
	switch spec {
	case "private":
		return nitrokit.TrustPrivateProxies(), nil
	case "none":
		return new(nitrokit.ProxyTrust), nil // the zero value trusts no peer
	default:
		return nitrokit.ParseTrustedProxies(spec)
	}
}

type server struct {
	dir     string
	log     *slog.Logger
	trust   *nitrokit.ProxyTrust
	limit   *byteLimiter // nil when -rate is 0
	traffic *traffic
	page    *template.Template
	assets  fs.FS
}

func newServer(dir string, trust *nitrokit.ProxyTrust, rate, burst float64, log *slog.Logger) (*server, error) {
	page, err := template.ParseFS(embedded, "web/index.html")
	if err != nil {
		return nil, err
	}
	assets, err := fs.Sub(embedded, "web")
	if err != nil {
		return nil, err
	}
	s := &server{
		dir:     dir,
		log:     log,
		trust:   trust,
		traffic: newTraffic(),
		page:    page,
		assets:  assets,
	}
	if rate > 0 {
		s.limit = newByteLimiter(rate, burst)
	}
	return s, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", nitrokit.Healthz)
	mux.Handle("GET /{$}", nitrokit.AccessLog(s.log, http.HandlerFunc(s.index)))
	mux.HandleFunc("GET /style.css", s.style)
	// The archive routes deliberately carry no access log: one map
	// session is hundreds of Range requests, and a line each buries
	// everything else. The rollup in traffic.report carries the numbers
	// that matter instead.
	mux.HandleFunc("GET /{file}", s.serveArchive)
	mux.HandleFunc("OPTIONS /{file}", archivePreflight)
	return mux
}

// announce logs what the binary found at startup. A missing or empty data
// directory is not a startup failure: the site has to be deployable
// before a 7.9 GB archive has finished copying into its volume.
func (s *server) announce() {
	list, err := s.archives()
	switch {
	case err != nil:
		s.log.Warn("no archive directory; every archive request will 404", "dir", s.dir, "err", err)
	case len(list) == 0:
		s.log.Warn("archive directory holds no "+archiveExt+" files", "dir", s.dir)
	default:
		for _, a := range list {
			s.log.Info("serving archive", "name", a.Name, "size", a.Human())
		}
	}
}

func (s *server) serveArchive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !strings.HasSuffix(name, archiveExt) {
		http.NotFound(w, r)
		return
	}

	// Set before any early return: a 404 or a 429 that a browser cannot
	// read is reported to the page as an opaque network error.
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Accept-Ranges")

	addr := s.trust.ClientIP(r).String()
	if s.limit != nil {
		if ok, retry := s.limit.allow(addr); !ok {
			s.traffic.denied(name)
			h.Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
			http.Error(w, "egress budget exhausted for this address; retry later", http.StatusTooManyRequests)
			return
		}
	}

	// Opened per request rather than held open from startup, so an
	// archive copied in after the process started is served without a
	// restart. os.Root confines the open to the data directory, which
	// makes traversal unrepresentable rather than merely filtered.
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()

	f, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(archiveMaxAge.Seconds())))
	// http.ServeContent sets Last-Modified and answers Range, If-Range,
	// and If-None-Match, but it generates no ETag. Without one a client
	// revalidating after max-age has only a timestamp, and pmtiles.js has
	// nothing to detect the archive changing mid-session with.
	h.Set("Etag", etag(info))

	counted := &countingWriter{ResponseWriter: w}
	http.ServeContent(counted, r, name, info.ModTime(), f)

	if s.limit != nil {
		// Charged after the fact against what actually went out, so a
		// range the client aborted costs only what it took. The bucket is
		// allowed to go negative: one oversized response overdraws the
		// address rather than being refused halfway through.
		s.limit.charge(addr, counted.n)
	}
	s.traffic.served(name, counted.n)
}

// archivePreflight answers the CORS preflight that pmtiles.js triggers by
// sending a Range header from another origin.
func archivePreflight(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.PathValue("file"), archiveExt) {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD")
	h.Set("Access-Control-Allow-Headers", "Range")
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	list, err := s.archives()
	if err != nil {
		// Not an error page: an empty list is the correct view of a site
		// whose archives have not been copied in yet.
		s.log.Warn("list archives", "dir", s.dir, "err", err)
	}
	nitrokit.NoCache(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.Execute(w, map[string]any{
		"Origin":   originOf(r),
		"Archives": list,
	}); err != nil {
		s.log.Error("render index", "err", err)
	}
}

func (s *server) style(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(assetMaxAge.Seconds())))
	http.ServeFileFS(w, r, s.assets, "style.css")
}

// originOf reconstructs the public origin so the page can show the URL a
// consumer pastes into its settings. Behind a TLS-terminating proxy the
// request itself is plain HTTP, so the scheme comes from the proxy.
func originOf(r *http.Request) string {
	scheme := cmp.Or(r.Header.Get("X-Forwarded-Proto"), "http")
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// archive describes one file in the data directory, for the index page.
type archive struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Human renders a size the way the plan's tables do: one decimal place,
// powers of 1024, because these files are measured in gigabytes and an
// exact byte count tells a reader nothing.
func (a archive) Human() string {
	const unit = 1024
	if a.Size < unit {
		return strconv.FormatInt(a.Size, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := a.Size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(a.Size)/float64(div), "KMGTPE"[exp])
}

// archives lists the data directory on every call rather than caching it,
// so copying a file in is all it takes to publish an archive.
func (s *server) archives() ([]archive, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var list []archive
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, archive{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	slices.SortFunc(list, func(a, b archive) int { return cmp.Compare(a.Name, b.Name) })
	return list, nil
}

// etag is a strong validator built from the size and modification time,
// which is what identifies a build of an archive: an extract writes a new
// file, so the pair changes whenever the bytes do. Hashing gigabytes at
// startup to do better is not worth the minutes it would cost.
func etag(info fs.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())
}

// countingWriter totals the body bytes a handler writes, which is both
// what the rate limiter charges and what the egress rollup reports.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// Unwrap keeps http.ResponseController working through this wrapper, so
// the write deadlines WriteBudget sets still reach the connection.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// traffic accumulates per-archive egress for periodic reporting. Counting
// here and logging a rollup keeps the archive routes out of the access
// log, where one map session would otherwise write hundreds of lines.
type traffic struct {
	mu sync.Mutex
	by map[string]*archiveTraffic
}

type archiveTraffic struct {
	requests int64
	bytes    int64
	denied   int64
}

func newTraffic() *traffic { return &traffic{by: map[string]*archiveTraffic{}} }

func (t *traffic) served(name string, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.entry(name)
	a.requests++
	a.bytes += n
}

func (t *traffic) denied(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entry(name).denied++
}

// entry returns the counter for name, creating it. Caller holds the mutex.
func (t *traffic) entry(name string) *archiveTraffic {
	a, ok := t.by[name]
	if !ok {
		a = new(archiveTraffic)
		t.by[name] = a
	}
	return a
}

// report logs one line per archive that saw traffic since the last call,
// then resets. Nothing is logged for an idle period.
func (t *traffic) report(log *slog.Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, a := range t.by {
		log.Info("egress", "archive", name, "requests", a.requests,
			"bytes", a.bytes, "denied", a.denied)
	}
	clear(t.by)
}

func (t *traffic) reportEvery(ctx context.Context, every time.Duration, log *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.report(log)
		}
	}
}
