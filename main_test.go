package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hammondus/nitrokit"
)

// archiveBody is stand-in content: the handler serves bytes and never
// parses them, so a real PMTiles header would test nothing extra.
const archiveBody = "0123456789abcdefghijklmnopqrstuvwxyz"

// slowRate is a thousand seconds per byte. nitrokit.Limiter keeps its
// clock unexported, so rather than injecting one, the rate-limit tests
// refill so slowly that a test run cannot measurably move the balance.
const slowRate = 0.001

// testServer returns a server over a data directory holding one archive,
// with its routes wired. rate of 0 disables the limiter.
func testServer(t *testing.T, rate, burst float64) (*server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "world-z8.pmtiles"), []byte(archiveBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret beside the data directory, for the traversal check.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.pmtiles"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := newServer(dir, new(nitrokit.ProxyTrust), rate, burst, log)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.routes()
}

func do(t *testing.T, h http.Handler, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestArchiveServesWholeFileWithCORS(t *testing.T) {
	_, h := testServer(t, 0, 0)
	res := do(t, h, "GET", "/world-z8.pmtiles", map[string]string{"Origin": "http://127.0.0.1:8484"})

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != archiveBody {
		t.Errorf("body = %q, want %q", body, archiveBody)
	}
	// Every header the logbook's cross-origin map depends on. Without
	// Content-Range exposed, pmtiles.js cannot read the response it just
	// received, and the failure surfaces as an opaque network error.
	want := map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Expose-Headers": "Content-Length, Content-Range, ETag, Accept-Ranges",
		"Accept-Ranges":                 "bytes",
		"Cache-Control":                 "public, max-age=86400",
		"Content-Type":                  "application/octet-stream",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if res.Header.Get("Etag") == "" {
		t.Error("no ETag; a revalidating client has only a timestamp, and pmtiles.js cannot detect the archive changing")
	}
	if strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Error("Cache-Control is immutable; the archive name is reused across rebuilds")
	}
}

func TestArchiveServesRange(t *testing.T) {
	_, h := testServer(t, 0, 0)
	res := do(t, h, "GET", "/world-z8.pmtiles", map[string]string{
		"Origin": "http://127.0.0.1:8484",
		"Range":  "bytes=0-15",
	})

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if want := archiveBody[:16]; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if want := "bytes 0-15/36"; res.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range = %q, want %q", res.Header.Get("Content-Range"), want)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header on the partial response")
	}
}

func TestArchiveRevalidatesWithETag(t *testing.T) {
	_, h := testServer(t, 0, 0)
	etag := do(t, h, "GET", "/world-z8.pmtiles", nil).Header.Get("Etag")

	res := do(t, h, "GET", "/world-z8.pmtiles", map[string]string{"If-None-Match": etag})
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for a matching ETag", res.StatusCode)
	}
}

func TestArchivePreflight(t *testing.T) {
	_, h := testServer(t, 0, 0)
	res := do(t, h, "OPTIONS", "/world-z8.pmtiles", map[string]string{
		"Origin":                         "http://127.0.0.1:8484",
		"Access-Control-Request-Method":  "GET",
		"Access-Control-Request-Headers": "range",
	})

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	want := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, HEAD",
		"Access-Control-Allow-Headers": "Range",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestArchiveNotFound(t *testing.T) {
	_, h := testServer(t, 0, 0)
	for _, target := range []string{
		"/missing.pmtiles",   // no such archive
		"/style.css.pmtiles", // suffix matches, file does not exist
		"/README.md",         // wrong extension
		"/sub/world-z8.pmtiles",
	} {
		if res := do(t, h, "GET", target, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", target, res.StatusCode)
		}
	}
}

// TestArchiveStaysInsideDataDirectory calls the handler directly with a
// traversing name, which the router's single-segment wildcard could never
// produce. It asserts the second line of defence: os.Root confines the
// open to the data directory.
func TestArchiveStaysInsideDataDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.pmtiles"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := newServer(dir, new(nitrokit.ProxyTrust), 0, 0, log)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("file", "../outside.pmtiles")
	w := httptest.NewRecorder()
	s.serveArchive(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; the handler read outside its data directory", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("the handler served a file from outside its data directory")
	}
}

func TestArchiveRateLimitChargesBytes(t *testing.T) {
	// A burst of two whole archives, so the third request is refused and
	// the refusal is driven by bytes rather than by a request count.
	_, h := testServer(t, slowRate, 2*float64(len(archiveBody)))

	for i := range 2 {
		if res := do(t, h, "GET", "/world-z8.pmtiles", nil); res.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, res.StatusCode)
		}
	}

	res := do(t, h, "GET", "/world-z8.pmtiles", nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the byte budget was spent", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on the 429")
	}
	// A refused request still has to be readable cross-origin, or the
	// browser reports it as an unexplained network failure.
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header on the 429")
	}
}

func TestSmallRequestsDoNotSpendTheBudgetLikeRequestCounting(t *testing.T) {
	// The same budget as the test above — two whole archives — spent
	// sixteen bytes at a time. All four range requests are served, where
	// a limiter counting requests with a burst of two would have refused
	// the third. This is the whole reason the limiter charges bytes: one
	// map session is a burst of small Range requests.
	_, h := testServer(t, slowRate, 2*float64(len(archiveBody)))
	for i := range 4 {
		res := do(t, h, "GET", "/world-z8.pmtiles", map[string]string{"Range": "bytes=0-15"})
		if res.StatusCode != http.StatusPartialContent {
			t.Fatalf("range request %d: status = %d, want 206", i+1, res.StatusCode)
		}
	}
}

func TestIndexListsArchives(t *testing.T) {
	_, h := testServer(t, 0, 0)
	res := do(t, h, "GET", "/", nil)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache: the page names the archives it links to", got)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "world-z8.pmtiles") {
		t.Error("the archive is not listed on the index page")
	}
}

// TestIndexWithNoArchives covers the deploy-day state: the binary has to
// serve before a multi-gigabyte archive has finished copying in.
func TestIndexWithNoArchives(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := newServer(filepath.Join(t.TempDir(), "absent"), new(nitrokit.ProxyTrust), 0, 0, log)
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()

	if res := do(t, h, "GET", "/", nil); res.StatusCode != http.StatusOK {
		t.Errorf("index status = %d, want 200 with no data directory", res.StatusCode)
	}
	if res := do(t, h, "GET", "/world-z8.pmtiles", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("archive status = %d, want 404 with no data directory", res.StatusCode)
	}
	s.announce() // must not panic on a missing directory
}

func TestStyleAndHealthz(t *testing.T) {
	_, h := testServer(t, 0, 0)

	res := do(t, h, "GET", "/style.css", nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("style status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("style Cache-Control = %q, want an hour: the URL carries no hash", got)
	}

	if res := do(t, h, "GET", "/healthz", nil); res.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", res.StatusCode)
	}
}

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{45 << 20, "45.0 MiB"},
		{8482000000, "7.9 GiB"},
	} {
		if got := (archive{Size: tc.size}).Human(); got != tc.want {
			t.Errorf("Human(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

func TestProxyTrust(t *testing.T) {
	for _, spec := range []string{"private", "none", "10.0.0.0/8,192.168.1.1"} {
		if _, err := proxyTrust(spec); err != nil {
			t.Errorf("proxyTrust(%q) = %v", spec, err)
		}
	}
	if _, err := proxyTrust("not-a-cidr"); err == nil {
		t.Error("proxyTrust accepted a malformed CIDR list")
	}
}

// TestArchiveRejectsBareOptions covers the request the CORS middleware
// passes through: an OPTIONS without Access-Control-Request-Method is an
// ordinary request, and answering it with gigabytes would be wrong.
func TestArchiveRejectsBareOptions(t *testing.T) {
	_, h := testServer(t, 0, 0)
	res := do(t, h, "OPTIONS", "/world-z8.pmtiles", nil)

	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow = %q, want the methods the route implements", got)
	}
	if n, _ := io.Copy(io.Discard, res.Body); n > 64 {
		t.Errorf("wrote %d bytes to a bare OPTIONS, want only the error text", n)
	}
}

// TestEgressCountsDistinctAddresses covers the operational question the
// count exists for: is the per-address budget actually per address?
func TestEgressCountsDistinctAddresses(t *testing.T) {
	s, h := testServer(t, 0, 0)

	for _, peer := range []string{"203.0.113.1:1", "203.0.113.1:2", "203.0.113.9:1"} {
		r := httptest.NewRequest("GET", "/world-z8.pmtiles", nil)
		r.RemoteAddr = peer
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	s.traffic.mu.Lock()
	defer s.traffic.mu.Unlock()
	a := s.traffic.by["world-z8.pmtiles"]
	if a.requests != 3 {
		t.Fatalf("requests = %d, want 3", a.requests)
	}
	// Two addresses, three requests: the port is not part of the identity.
	if got := len(a.addresses); got != 2 {
		t.Errorf("addresses = %d, want 2", got)
	}
}

// TestEgressAddressesFollowForwardedFor is the deployed case. Behind a
// proxy that sets X-Forwarded-For, the count must follow the real callers;
// if it tracked the peer, every client would share one egress budget.
func TestEgressAddressesFollowForwardedFor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "world-z8.pmtiles"), []byte(archiveBody), 0o644); err != nil {
		t.Fatal(err)
	}
	trust, err := proxyTrust("private")
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(dir, trust, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()

	for _, client := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		r := httptest.NewRequest("GET", "/world-z8.pmtiles", nil)
		r.RemoteAddr = "172.18.0.2:5000" // the proxy, on a container network
		r.Header.Set("X-Forwarded-For", client)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	s.traffic.mu.Lock()
	defer s.traffic.mu.Unlock()
	if got := len(s.traffic.by["world-z8.pmtiles"].addresses); got != 3 {
		t.Errorf("addresses = %d, want 3; the count is tracking the proxy, not the clients", got)
	}
}

func TestEgressAddressSetIsBounded(t *testing.T) {
	tr := newTraffic()
	for i := range maxTrackedAddresses + 500 {
		tr.served("a.pmtiles", fmt.Sprintf("198.51.100.%d", i), 1)
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	a := tr.by["a.pmtiles"]
	if len(a.addresses) != maxTrackedAddresses {
		t.Errorf("tracked %d addresses, want the cap of %d", len(a.addresses), maxTrackedAddresses)
	}
	if !a.capped {
		t.Error("the rollup does not mark the count as capped, so a reader would read it as exact")
	}
	if a.requests != maxTrackedAddresses+500 {
		t.Error("the cap dropped requests as well as addresses")
	}
}
