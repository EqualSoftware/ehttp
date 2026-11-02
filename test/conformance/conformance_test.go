package conformance_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

type cResp struct {
	Status int
	Header http.Header
	Body   string
	URL    string // server base URL
}

type cWant func(t testing.TB, basis string, r *cResp)

// conformanceCase is one row of the conformance table. basis must name the exact
// spec section that makes want the correct behavior.
type conformanceCase struct {
	name     string
	basis    string
	knownBug string // name of the open defect while the bug reproduces
	fixture  string
	method   string
	path     string
	hdr      map[string]string // values may use {etag} {etag-strong} {lm} {lm-1h} {lm+1h}
	ref      bool              // also check the expectation against http.ServeContent
	want     cWant
}

// ---- want constructors ----------------------------------------------------

func cAll(ws ...cWant) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		for _, w := range ws {
			w(t, basis, r)
		}
	}
}

func cStatus(codes ...int) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		for _, c := range codes {
			if r.Status == c {
				return
			}
		}

		t.Errorf("[%s] status = %d, want one of %v (body %q)", basis, r.Status, codes, r.Body)
	}
}

func cHdr(name, want string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()
		ehttptest.Eq(t, basis+" header "+name, r.Header.Get(name), want)
	}
}

func cNoHdr(name string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		_, ok := r.Header[http.CanonicalHeaderKey(name)]
		ehttptest.True(t, basis, !ok, "header %s present: %q", name, r.Header.Values(name))
	}
}

func cHasHdr(name string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()
		ehttptest.True(t, basis, r.Header.Get(name) != "", "header %s missing", name)
	}
}

func cTokens(h http.Header, name string) []string {
	var out []string

	for _, line := range h.Values(name) {
		for _, p := range strings.Split(line, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				out = append(out, p)
			}
		}
	}

	return out
}

func cHasToken(name, token string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		for _, tok := range cTokens(r.Header, name) {
			if tok == strings.ToLower(token) || strings.HasPrefix(tok, strings.ToLower(token)+"=") {
				return
			}
		}

		t.Errorf("[%s] header %s = %q, missing token %q", basis, name, r.Header.Values(name), token)
	}
}

func cLacksToken(name, token string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		for _, tok := range cTokens(r.Header, name) {
			if tok == strings.ToLower(token) {
				t.Errorf("[%s] header %s = %q must not contain %q", basis, name, r.Header.Values(name), token)
			}
		}
	}
}

// cNotSharedCacheable checks that a shared cache must not store the response
// (RFC 9111 §3, §5.2.2.5, §5.2.2.10): no Cache-Control, or private, or
// no-store is fine; public, s-maxage or a bare max-age is not.
func cNotSharedCacheable() cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		toks := cTokens(r.Header, "Cache-Control")
		for _, tok := range toks {
			if tok == "private" || tok == "no-store" {
				return
			}
		}

		for _, tok := range toks {
			if tok == "public" || strings.HasPrefix(tok, "s-maxage=") || strings.HasPrefix(tok, "max-age=") {
				t.Errorf("[%s] Cache-Control = %q lets a shared cache store the response", basis, r.Header.Values("Cache-Control"))
				return
			}
		}
	}
}

func cBody(want string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()
		ehttptest.Eq(t, basis+" body", r.Body, want)
	}
}

// cMultiOrFull accepts the two RFC 9110 §14.2/§14.6 behaviors for several ranges:
// ignore the header (200, whole file) or answer 206 multipart/byteranges.
func cMultiOrFull(full string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		if r.Status == 200 && r.Body == full {
			return
		}

		if r.Status == 206 && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/byteranges") {
			return
		}

		t.Errorf("[%s] status=%d content-type=%q content-range=%q body=%q: want 200 full or 206 multipart/byteranges",
			basis, r.Status, r.Header.Get("Content-Type"), r.Header.Get("Content-Range"), r.Body)
	}
}

// cSameAsGet checks RFC 9110 §9.3.2: HEAD returns the header fields GET would.
func cSameAsGet(path string, names ...string) cWant {
	return func(t testing.TB, basis string, r *cResp) {
		t.Helper()

		get, _ := fixtures.Fetch(t, http.MethodGet, r.URL+path, nil)

		for _, n := range names {
			ehttptest.Eq(t, basis+" header "+n+" vs GET", r.Header.Get(n), get.Header.Get(n))
		}
	}
}

// ---- fixtures -------------------------------------------------------------

const (
	cFileBody = "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789"
	cVaryUA   = "User-Agent"
)

var (
	cModFrac = time.Date(2024, 1, 2, 3, 4, 5, 678_000_000, time.UTC) // typical filesystem mtime
	cModSec  = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cLastMod = time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
)

func cWriteFile(t testing.TB, dir, name string, mod time.Time) string {
	t.Helper()

	p := filepath.Join(dir, name)

	if err := os.WriteFile(p, []byte(cFileBody), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}

	return p
}

// cMainServer holds every non-file, non-CORS route used by the table.
func cMainServer() *ehttp.Server {
	srv := ehttp.NewServer()
	text := func(s string) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String(s) }
	}

	srv.GET("/hello", text("hello"))
	srv.GET("/only", text("only"))
	srv.GET("/shared", text("get"))
	srv.POST("/shared", text("post"))

	srv.GET("/vary-ae-then-accept", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Vary("Accept")
		res.String("v")
	}, middleware.AcceptEncoding())
	srv.GET("/vary-accept-then-ae", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Vary("Accept")
		res.Vary("Accept-Encoding")
		res.String("v")
	})
	srv.GET("/vary-dup", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.Vary("Accept-Encoding")
		res.Vary("accept-encoding")
		res.Vary(cVaryUA)
		res.String("v")
	})

	srv.GET("/cache", text("c"), middleware.Cache(time.Minute))
	srv.GET("/nocache", text("c"), middleware.Cache(0))
	srv.GET("/err404", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(404) }, middleware.ErrorCache(time.Minute))
	srv.GET("/err401", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(401) }, middleware.ErrorCache(time.Minute))
	srv.GET("/err429", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(429) }, middleware.ErrorCache(time.Minute))
	srv.GET("/ok-errcache", text("ok"), middleware.ErrorCache(time.Minute))

	srv.GET("/etag", text("etag-body"), middleware.WeakEtag())
	srv.GET("/lm", text("lm-body"), middleware.LastModified(cLastMod))
	srv.GET("/lmetag", text("lmetag-body"), middleware.LastModified(cLastMod), middleware.WeakEtag())

	return srv
}

// cFileServer serves two files through SendFile.
func cFileServer(t testing.TB) (srv *ehttp.Server, frac string) {
	dir := t.TempDir()
	frac = cWriteFile(t, dir, "frac.txt", cModFrac)
	sec := cWriteFile(t, dir, "sec.txt", cModSec)

	srv = ehttp.NewServer()
	srv.GET("/file", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendFile(frac) })
	srv.GET("/file-sec", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendFile(sec) })

	return srv, frac
}

func cFixture(t testing.TB, name string) (srv *ehttp.Server, frac string) {
	switch name {
	case "main":
		return cMainServer(), ""
	case "file":
		return cFileServer(t)
	case "cors-star":
		return fixtures.CorsServer(t, nil), ""
	case "cors-list":
		return fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA, fixtures.OriginB}}), ""
	case "cors-list-creds":
		return fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}, Credentials: true}), ""
	case "cors-restricted":
		return fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}, Methods: []string{"GET"}, Headers: []string{"Content-Type"}}), ""
	case "cors-maxage":
		return fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}, CacheDuration: 10 * time.Minute}), ""
	}

	t.Fatalf("unknown fixture %q", name)

	return nil, ""
}

// cReference serves the same file with http.ServeContent, the behavior the
// target for SendFile to match.
func cReference(t testing.TB, frac, etag string) *httptest.Server {
	h := func(w http.ResponseWriter, r *http.Request) {
		p := frac
		mod := cModFrac

		if strings.HasSuffix(r.URL.Path, "-sec") {
			p, mod = filepath.Join(filepath.Dir(frac), "sec.txt"), cModSec
		}

		f, err := os.Open(p)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()

		w.Header().Set("Etag", etag)
		http.ServeContent(w, r, "x.txt", mod, f)
	}

	ts := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(ts.Close)

	return ts
}

// ---- runner ---------------------------------------------------------------

func cExpand(t testing.TB, base, path string, hdr map[string]string) http.Header {
	out := http.Header{}

	needs := false
	for _, v := range hdr {
		needs = needs || strings.Contains(v, "{")
	}

	var etag, lm string

	if needs {
		resp, _ := fixtures.Fetch(t, http.MethodGet, base+path, nil)
		etag, lm = resp.Header.Get("Etag"), resp.Header.Get("Last-Modified")
	}

	lmt, _ := http.ParseTime(lm)
	rep := strings.NewReplacer(
		"{etag-strong}", strings.TrimPrefix(etag, "W/"),
		"{etag}", etag,
		"{lm-1h}", lmt.Add(-time.Hour).UTC().Format(http.TimeFormat),
		"{lm+1h}", lmt.Add(time.Hour).UTC().Format(http.TimeFormat),
		"{lm}", lm,
	)

	for k, v := range hdr {
		out.Set(k, rep.Replace(v))
	}

	return out
}

func cRun(t testing.TB, base, path, method string, hdr http.Header) *cResp {
	resp, body := fixtures.Fetch(t, method, base+path, hdr)

	return &cResp{Status: resp.StatusCode, Header: resp.Header, Body: body, URL: base}
}

func TestCConformance(t *testing.T) {
	for _, tc := range conformanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.basis == "" {
				t.Fatal("conformance case without basis")
			}

			srv, _ := cFixture(t, tc.fixture)
			ts := fixtures.Start(t, srv)
			hdr := cExpand(t, ts.URL, tc.path, tc.hdr)
			label := tc.basis

			check := func(t testing.TB) {
				t.Helper()
				tc.want(t, label, cRun(t, ts.URL, tc.path, tc.method, hdr))
			}

			if tc.knownBug != "" {
				ehttptest.KnownBug(t, tc.knownBug, check)
				return
			}

			check(t)
		})

		if tc.ref {
			t.Run(tc.name+"/reference-ServeContent", func(t *testing.T) {
				srv, frac := cFixture(t, tc.fixture)
				ts := fixtures.Start(t, srv)
				resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+tc.path, nil)
				ref := cReference(t, frac, resp.Header.Get("Etag"))
				hdr := cExpand(t, ts.URL, tc.path, tc.hdr)

				tc.want(t, "net/http ServeContent agrees with table: "+tc.basis, cRun(t, ref.URL, tc.path, tc.method, hdr))
			})
		}
	}
}

func conformanceCases() []conformanceCase {
	const (
		h932  = "RFC 9110 §9.3.2 HEAD"
		h937  = "RFC 9110 §9.3.7 OPTIONS"
		h1556 = "RFC 9110 §15.5.6 405 Method Not Allowed"
		h1411 = "RFC 9110 §14.1.2 byte ranges"
		h1417 = "RFC 9110 §15.5.17 416 Range Not Satisfiable"
		h146  = "RFC 9110 §14.6 multipart/byteranges"
		h1315 = "RFC 9110 §13.1.5 If-Range"
		h1311 = "RFC 9110 §13.1.1 If-Match"
		h1312 = "RFC 9110 §13.1.2 If-None-Match"
		h1313 = "RFC 9110 §13.1.3 If-Modified-Since"
		h1314 = "RFC 9110 §13.1.4 If-Unmodified-Since"
		h1322 = "RFC 9110 §13.2.2 precedence"
		h1255 = "RFC 9110 §12.5.5 Vary"
		h9111 = "RFC 9111 §5.2 Cache-Control"
		fetch = "Fetch §3.2.3 CORS protocol, HTTP responses"
		fcach = "Fetch §3.2.7 CORS protocol and HTTP caches"
		fcred = "Fetch §3.2.5 CORS protocol and credentials"
	)

	get := http.MethodGet
	head := http.MethodHead
	opts := http.MethodOptions
	rng := func(v string) map[string]string { return map[string]string{"Range": v} }
	hd := func(k, v string) map[string]string { return map[string]string{k: v} }
	cors := func(origin string) map[string]string { return map[string]string{"Origin": origin} }
	// Browsers always send Sec-Fetch-Site on a cross-site preflight.
	preflight := func(origin, method string) map[string]string {
		return map[string]string{"Origin": origin, "Access-Control-Request-Method": method, "Sec-Fetch-Site": "cross-site"}
	}

	return []conformanceCase{
		// ---- HEAD / OPTIONS / 405 -------------------------------------------------
		{name: "HEAD mirrors GET headers, no body", basis: h932, fixture: "main", method: head, path: "/hello",
			want: cAll(cStatus(200), cBody(""), cHdr("Content-Length", "5"), cSameAsGet("/hello", "Content-Length", "Content-Type"))},
		{name: "HEAD on unknown path is 404", basis: h932, fixture: "main", method: head, path: "/nope",
			want: cAll(cStatus(404), cBody(""))},
		{name: "HEAD through WeakEtag keeps GET Content-Length", basis: h932, fixture: "main", method: head, path: "/etag",
			want: cAll(cStatus(200), cBody(""), cHdr("Content-Length", "9"))},
		{name: "405 carries Allow", basis: h1556, fixture: "main", method: http.MethodPost, path: "/only",
			want: cAll(cStatus(405), cHasHdr("Allow"), cHasToken("Allow", "GET"))},
		{name: "405 Allow lists every method on the path", basis: h1556, fixture: "main", method: http.MethodDelete, path: "/shared",
			want: cAll(cStatus(405), cHasToken("Allow", "GET"), cHasToken("Allow", "POST"))},
		{name: "method registered after another on same path is routed", basis: h1556, fixture: "main", method: http.MethodPost, path: "/shared",
			want: cAll(cStatus(200), cBody("post"))},
		{name: "OPTIONS answers 2xx", basis: h937, fixture: "main", method: opts, path: "/only",
			want: cStatus(200, 204)},
		{name: "OPTIONS advertises Allow (SHOULD)", basis: h937, fixture: "main", method: opts, path: "/only",
			want: cAll(cHasToken("Allow", "GET"), cHasToken("Allow", "OPTIONS"))},

		// ---- SendFile: Range (RFC 9110 §14) ---------------------------------------
		{name: "file: no Range is 200 with Accept-Ranges", basis: "RFC 9110 §14.3 Accept-Ranges", fixture: "file", method: get, path: "/file", ref: true,
			want: cAll(cStatus(200), cHdr("Accept-Ranges", "bytes"), cBody(cFileBody))},
		{name: "file: bytes=10-19", basis: h1411, fixture: "file", method: get, path: "/file", hdr: rng("bytes=10-19"), ref: true,
			want: cAll(cStatus(206), cHdr("Content-Range", "bytes 10-19/100"), cBody("0123456789"))},
		{name: "file: bytes=0-0 is the first byte only", basis: h1411, fixture: "file", method: get, path: "/file", hdr: rng("bytes=0-0"), ref: true,
			want: cAll(cStatus(206), cHdr("Content-Range", "bytes 0-0/100"), cBody("0"))},
		{name: "file: bytes=90- open-ended", basis: h1411, fixture: "file", method: get, path: "/file", hdr: rng("bytes=90-"), ref: true,
			want: cAll(cStatus(206), cHdr("Content-Range", "bytes 90-99/100"), cBody("0123456789"))},
		{name: "file: bytes=-5 suffix", basis: h1411, fixture: "file", method: get, path: "/file", hdr: rng("bytes=-5"), ref: true,
			want: cAll(cStatus(206), cHdr("Content-Range", "bytes 95-99/100"), cBody("56789"))},
		{name: "file: last-pos beyond size is clamped", basis: h1411, fixture: "file", method: get, path: "/file", hdr: rng("bytes=95-999"), ref: true,
			want: cAll(cStatus(206), cHdr("Content-Range", "bytes 95-99/100"), cBody("56789"))},
		{name: "file: first-pos beyond size is 416", basis: h1417, fixture: "file", method: get, path: "/file", hdr: rng("bytes=100-110"), ref: true,
			want: cStatus(416)},
		{name: "file: 416 has Content-Range bytes */size", basis: h1417, fixture: "file", method: get, path: "/file", hdr: rng("bytes=100-110"), ref: true,
			want: cAll(cStatus(416), cHdr("Content-Range", "bytes */100"))},
		{name: "file: last-pos < first-pos is invalid, ignore (200) or 416", basis: "RFC 9110 §14.2 invalid ranges-specifier", fixture: "file", method: get, path: "/file", hdr: rng("bytes=5-2"),
			want: cStatus(200, 416)},
		{name: "file: Range bytes=500-100 is 416 with Content-Range", basis: h1417 + " and RFC 9110 §14.1.1 last-pos < first-pos is invalid", fixture: "file", method: get, path: "/file", hdr: rng("bytes=500-100"),
			want: cAll(cStatus(416), cHdr("Content-Range", "bytes */100"))},
		{name: "file: Range bytes=abc is 416 with Content-Range", basis: h1417 + " and RFC 9110 §14.1.1 range-spec syntax", fixture: "file", method: get, path: "/file", hdr: rng("bytes=abc"),
			want: cAll(cStatus(416), cHdr("Content-Range", "bytes */100"))},
		{name: "file: Range with an unknown unit is ignored (200)", basis: "RFC 9110 §14.2 an origin server MUST ignore a Range with an unknown unit", fixture: "file", method: get, path: "/file", hdr: rng("items=0-5"),
			want: cAll(cStatus(200), cBody(cFileBody))},
		{name: "file: several ranges answered whole or multipart", basis: h146, fixture: "file", method: get, path: "/file", hdr: rng("bytes=0-9,20-29"), ref: true,
			want: cMultiOrFull(cFileBody)},
		{name: "file: HEAD has Content-Length, no body", basis: h932, fixture: "file", method: head, path: "/file", ref: true,
			want: cAll(cStatus(200), cBody(""), cHdr("Content-Length", "100"), cHdr("Accept-Ranges", "bytes"))},

		// ---- SendFile: If-Range (§13.1.5) -----------------------------------------
		{name: "file: If-Range date equal to Last-Modified honors Range", basis: h1315, fixture: "file", method: get, path: "/file", hdr: map[string]string{"Range": "bytes=0-4", "If-Range": "{lm}"}, ref: true,
			want: cAll(cStatus(206), cBody("01234"))},
		{name: "file: If-Range stale date ignores Range", basis: h1315, fixture: "file", method: get, path: "/file", hdr: map[string]string{"Range": "bytes=0-4", "If-Range": "{lm-1h}"}, ref: true,
			want: cAll(cStatus(200), cBody(cFileBody))},
		{name: "file: If-Range with weak validator ignores Range", basis: h1315 + " strong comparison", fixture: "file", method: get, path: "/file", hdr: map[string]string{"Range": "bytes=0-4", "If-Range": "{etag}"}, ref: true,
			want: cAll(cStatus(200), cBody(cFileBody))},

		// ---- SendFile: conditionals (§13.1, §13.2.2) ------------------------------
		{name: "file: If-None-Match same weak tag is 304", basis: h1312, fixture: "file", method: get, path: "/file", hdr: hd("If-None-Match", "{etag}"), ref: true,
			want: cAll(cStatus(304), cBody(""), cHasHdr("Etag"))},
		{name: "file: If-None-Match different tag is 200", basis: h1312, fixture: "file", method: get, path: "/file", hdr: hd("If-None-Match", `W/"other"`), ref: true,
			want: cAll(cStatus(200), cBody(cFileBody))},
		{name: "file: If-None-Match uses weak comparison (strong form of same tag)", basis: h1312 + " weak comparison", fixture: "file", method: get, path: "/file", hdr: hd("If-None-Match", "{etag-strong}"), ref: true,
			want: cStatus(304)},
		{name: "file: If-None-Match list containing the tag", basis: h1312 + " list", fixture: "file", method: get, path: "/file", hdr: hd("If-None-Match", `"zzz", {etag}`), ref: true,
			want: cStatus(304)},
		{name: "file: If-None-Match * on existing resource", basis: h1312 + " *", fixture: "file", method: get, path: "/file", hdr: hd("If-None-Match", "*"), ref: true,
			want: cStatus(304)},
		{name: "file: If-Modified-Since == Last-Modified (fractional mtime) is 304", basis: h1313, fixture: "file", method: get, path: "/file", hdr: hd("If-Modified-Since", "{lm}"), ref: true,
			want: cStatus(304)},
		{name: "file: If-Modified-Since == Last-Modified (whole-second mtime) is 304", basis: h1313, fixture: "file", method: get, path: "/file-sec", hdr: hd("If-Modified-Since", "{lm}"), ref: true,
			want: cStatus(304)},
		{name: "file: If-Modified-Since later date is 304", basis: h1313, fixture: "file", method: get, path: "/file", hdr: hd("If-Modified-Since", "{lm+1h}"), ref: true,
			want: cStatus(304)},
		{name: "file: If-Modified-Since older date is 200", basis: h1313, fixture: "file", method: get, path: "/file", hdr: hd("If-Modified-Since", "{lm-1h}"), ref: true,
			want: cAll(cStatus(200), cBody(cFileBody))},
		{name: "file: invalid If-Modified-Since is ignored", basis: h1313 + " (invalid date ignored)", fixture: "file", method: get, path: "/file", hdr: hd("If-Modified-Since", "yesterday"), ref: true,
			want: cStatus(200)},
		{name: "file: If-None-Match wins over If-Modified-Since", basis: h1322, fixture: "file", method: get, path: "/file", hdr: map[string]string{"If-None-Match": `W/"other"`, "If-Modified-Since": "{lm+1h}"}, ref: true,
			want: cAll(cStatus(200), cBody(cFileBody))},
		{name: "file: If-Match that does not match is 412", basis: h1311, fixture: "file", method: get, path: "/file", hdr: hd("If-Match", `"nope"`), ref: true,
			want: cStatus(412)},
		{name: "file: If-Match * on existing resource is 200", basis: h1311, fixture: "file", method: get, path: "/file", hdr: hd("If-Match", "*"), ref: true,
			want: cStatus(200)},
		{name: "file: If-Unmodified-Since before Last-Modified is 412", basis: h1314, fixture: "file", method: get, path: "/file-sec", hdr: hd("If-Unmodified-Since", "{lm-1h}"), ref: true,
			want: cStatus(412)},
		{name: "file: If-Unmodified-Since after Last-Modified is 200", basis: h1314, fixture: "file", method: get, path: "/file-sec", hdr: hd("If-Unmodified-Since", "{lm+1h}"), ref: true,
			want: cStatus(200)},

		// ---- WeakEtag / LastModified middleware (§8.8.3, §13) ---------------------
		{name: "WeakEtag: 200 carries a weak ETag", basis: "RFC 9110 §8.8.3 ETag", fixture: "main", method: get, path: "/etag",
			want: cAll(cStatus(200), cBody("etag-body"), func(t testing.TB, b string, r *cResp) {
				ehttptest.True(t, b, strings.HasPrefix(r.Header.Get("Etag"), `W/"`), "etag = %q", r.Header.Get("Etag"))
			})},
		{name: "WeakEtag: If-None-Match same tag is 304 with ETag", basis: h1312, fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", "{etag}"),
			want: cAll(cStatus(304), cBody(""), cHasHdr("Etag"))},
		{name: "WeakEtag: If-None-Match strong form (weak comparison)", basis: h1312 + " weak comparison", fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", "{etag-strong}"),
			want: cStatus(304)},
		{name: "WeakEtag: If-None-Match different tag is 200", basis: h1312, fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", `W/"other"`),
			want: cAll(cStatus(200), cBody("etag-body"))},
		{name: "WeakEtag: If-None-Match list containing the tag", basis: h1312 + " list", fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", `"zzz", {etag}`),
			want: cStatus(304)},
		{name: "WeakEtag: If-None-Match * on existing resource", basis: h1312 + " *", fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", "*"),
			want: cStatus(304)},
		{name: "WeakEtag: If-None-Match with an unquoted tag is 400", basis: "RFC 9110 §8.8.3 entity-tag is a quoted string, §15.5.1 400", fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", "abc"),
			want: cStatus(400)},
		{name: "WeakEtag: If-None-Match * mixed with tags is 400", basis: "RFC 9110 §13.1.2 \"*\" is the only member when present, §15.5.1 400", fixture: "main", method: get, path: "/etag", hdr: hd("If-None-Match", `*, "a"`),
			want: cStatus(400)},
		{name: "LastModified: Last-Modified on 200", basis: "RFC 9110 §8.8.2 Last-Modified", fixture: "main", method: get, path: "/lm",
			want: cAll(cStatus(200), cHdr("Last-Modified", cLastMod.Format(http.TimeFormat)), cBody("lm-body"))},
		{name: "LastModified: If-Modified-Since == Last-Modified is 304", basis: h1313, fixture: "main", method: get, path: "/lm", hdr: hd("If-Modified-Since", "{lm}"),
			want: cAll(cStatus(304), cBody(""))},
		{name: "LastModified: If-Modified-Since older is 200", basis: h1313, fixture: "main", method: get, path: "/lm", hdr: hd("If-Modified-Since", "{lm-1h}"),
			want: cAll(cStatus(200), cBody("lm-body"))},
		{name: "LastModified+WeakEtag: INM mismatch beats IMS match", basis: h1322, fixture: "main", method: get, path: "/lmetag", hdr: map[string]string{"If-None-Match": `W/"other"`, "If-Modified-Since": "{lm+1h}"},
			want: cAll(cStatus(200), cBody("lmetag-body"))},

		// ---- Vary (§12.5.5) -------------------------------------------------------
		{name: "Vary keeps Accept next to Accept-Encoding", basis: h1255, fixture: "main", method: get, path: "/vary-ae-then-accept",
			want: cAll(cHasToken("Vary", "Accept-Encoding"), cHasToken("Vary", "Accept"))},
		{name: "Vary: Accept then Accept-Encoding keeps both", basis: h1255, fixture: "main", method: get, path: "/vary-accept-then-ae",
			want: cAll(cHasToken("Vary", "Accept"), cHasToken("Vary", "Accept-Encoding"))},
		{name: "Vary does not duplicate a header name (case-insensitive)", basis: h1255, fixture: "main", method: get, path: "/vary-dup",
			want: func(t testing.TB, b string, r *cResp) {
				n := 0
				for _, tok := range cTokens(r.Header, "Vary") {
					if tok == "accept-encoding" {
						n++
					}
				}
				ehttptest.Eq(t, b+" count of accept-encoding", n, 1)
				ehttptest.True(t, b, len(cTokens(r.Header, "Vary")) == 2, "vary = %q", r.Header.Values("Vary"))
			}},

		// ---- Cache-Control (RFC 9111 §5.2) ----------------------------------------
		{name: "Cache(1m) sets max-age", basis: h9111 + ".2.1 max-age", fixture: "main", method: get, path: "/cache",
			want: cHasToken("Cache-Control", "max-age=60")},
		{name: "Cache(0) sets no-store", basis: h9111 + ".2.5 no-store", fixture: "main", method: get, path: "/nocache",
			want: cHasToken("Cache-Control", "no-store")},
		{name: "Cache without Authorization stays cacheable", basis: "RFC 9111 §3.5 (a request without Authorization stays cacheable)", fixture: "main", method: get, path: "/cache",
			want: cAll(cHasToken("Cache-Control", "max-age=60"), cHasToken("Cache-Control", "stale-if-error=60"))},
		{name: "Cache with Authorization is not public", basis: "RFC 9111 §3.5", fixture: "main", method: get, path: "/cache", hdr: hd("Authorization", "Bearer x"),
			want: cLacksToken("Cache-Control", "public")},
		{name: "ErrorCache caches 404", basis: "RFC 9110 §15.5.5 (404 is cacheable by default)", fixture: "main", method: get, path: "/err404",
			want: cAll(cStatus(404), cHasToken("Cache-Control", "max-age=60"))},
		{name: "ErrorCache leaves 2xx alone", basis: "ErrorCache contract", fixture: "main", method: get, path: "/ok-errcache",
			want: cAll(cStatus(200), cNoHdr("Cache-Control"))},
		{name: "ErrorCache does not let a shared cache store 401", basis: "RFC 9111 §3 + §5.2.2.5", fixture: "main", method: get, path: "/err401",
			want: cAll(cStatus(401), cNotSharedCacheable())},
		{name: "ErrorCache sends no-store on 429", basis: "RFC 6585 §4 (MUST NOT be stored by a cache)", fixture: "main", method: get, path: "/err429",
			want: cAll(cStatus(429), cHasToken("Cache-Control", "no-store"))},

		// ---- CORS (Fetch Standard) ------------------------------------------------
		{name: "CORS list: allowed origin is echoed", basis: fetch, fixture: "cors-list", method: get, path: "/api", hdr: cors(fixtures.OriginA),
			want: cAll(cStatus(200), cHdr("Access-Control-Allow-Origin", fixtures.OriginA))},
		{name: "CORS list: second allowed origin is echoed", basis: fetch, fixture: "cors-list", method: get, path: "/api", hdr: cors(fixtures.OriginB),
			want: cHdr("Access-Control-Allow-Origin", fixtures.OriginB)},
		{name: "CORS list: Vary: Origin on CORS response", basis: fcach, fixture: "cors-list", method: get, path: "/api", hdr: cors(fixtures.OriginA),
			want: cHasToken("Vary", "Origin")},
		{name: "CORS list: Vary: Origin even without Origin header", basis: fcach, fixture: "cors-list", method: get, path: "/api",
			want: cHasToken("Vary", "Origin")},
		{name: "CORS list: origin outside the list gets no ACAO", basis: fetch, fixture: "cors-list", method: get, path: "/api", hdr: cors(fixtures.OriginEvil),
			want: cNoHdr("Access-Control-Allow-Origin")},
		{name: "CORS star: static * or echo+Vary", basis: fcach, fixture: "cors-star", method: get, path: "/api", hdr: cors(fixtures.OriginA),
			want: func(t testing.TB, b string, r *cResp) {
				acao := r.Header.Get("Access-Control-Allow-Origin")
				if acao == "*" {
					return
				}
				cAll(cHdr("Access-Control-Allow-Origin", fixtures.OriginA), cHasToken("Vary", "Origin"))(t, b, r)
			}},
		{name: "CORS: Allow-Credentials only ever \"true\"", basis: fcred, fixture: "cors-list", method: get, path: "/api", hdr: cors(fixtures.OriginA),
			want: func(t testing.TB, b string, r *cResp) {
				if v, ok := r.Header["Access-Control-Allow-Credentials"]; ok {
					ehttptest.Eq(t, b, v[0], "true")
				}
			}},
		{name: "CORS creds: Allow-Credentials true for listed origin", basis: fcred, fixture: "cors-list-creds", method: get, path: "/api", hdr: cors(fixtures.OriginA),
			want: cAll(cHdr("Access-Control-Allow-Origin", fixtures.OriginA), cHdr("Access-Control-Allow-Credentials", "true"))},
		{name: "CORS preflight: allowed origin and method", basis: "Fetch §4.8 CORS-preflight fetch (ok status)", fixture: "cors-list", method: opts, path: "/api", hdr: preflight(fixtures.OriginA, "GET"),
			want: cAll(cStatus(200, 204), cHdr("Access-Control-Allow-Origin", fixtures.OriginA), cHasHdr("Access-Control-Allow-Methods"))},
		{name: "CORS preflight: listed origin without Sec-Fetch-Site", basis: "Fetch §3.2.2 + the origin allowlist", fixture: "cors-list", method: opts, path: "/api",
			hdr:  map[string]string{"Origin": fixtures.OriginA, "Access-Control-Request-Method": "GET"},
			want: cStatus(200, 204)},
		{name: "CORS preflight: Vary lists request-dependent headers", basis: fcach, fixture: "cors-list", method: opts, path: "/api", hdr: preflight(fixtures.OriginA, "GET"),
			want: cAll(cHasToken("Vary", "Origin"), cHasToken("Vary", "Access-Control-Request-Method"), cHasToken("Vary", "Access-Control-Request-Headers"))},
		{name: "CORS preflight: foreign origin is rejected", basis: "Fetch §4.8 + the origin allowlist", fixture: "cors-list", method: opts, path: "/api", hdr: preflight(fixtures.OriginEvil, "GET"),
			want: cStatus(403)},
		{name: "CORS preflight: method outside Allow-Methods rejected", basis: "Fetch §4.8 CORS-preflight fetch", fixture: "cors-restricted", method: opts, path: "/api", hdr: preflight(fixtures.OriginA, "DELETE"),
			want: cStatus(403)},
		{name: "CORS preflight: allowed request header", basis: "Fetch §4.8 CORS-preflight fetch", fixture: "cors-restricted", method: opts, path: "/api",
			hdr:  map[string]string{"Origin": fixtures.OriginA, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "content-type", "Sec-Fetch-Site": "cross-site"},
			want: cStatus(200, 204)},
		{name: "CORS preflight: disallowed request header rejected", basis: "Fetch §4.8 CORS-preflight fetch", fixture: "cors-restricted", method: opts, path: "/api",
			hdr:  map[string]string{"Origin": fixtures.OriginA, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "x-evil", "Sec-Fetch-Site": "cross-site"},
			want: cStatus(403)},
		{name: "CORS preflight: Max-Age from CacheDuration", basis: fetch + " Access-Control-Max-Age", fixture: "cors-maxage", method: opts, path: "/api", hdr: preflight(fixtures.OriginA, "GET"),
			want: cHdr("Access-Control-Max-Age", fmt.Sprint(int((10 * time.Minute).Seconds())))},
	}
}
