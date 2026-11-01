// C5 - fuzzing with invariants. Package ehttp (internal) so the targets can
// reach route.match, fileParseRange, fileIsNotModified, corsAcceptOrigin.
//
// How known bugs are handled: a fuzz target cannot be an expected failure.
// When fuzzing finds a bug that already has an ID, the crasher stays in
// testdata/fuzz/<Target>/ as a permanent seed, the fuzz function skips the
// known-bug domain (see the *Known helpers), and a regular test below
// reproduces the bug through ehttptest.KnownBug. When the bug is fixed, the
// KnownBug test fails, the marker is removed and the skip helper is deleted.
package ehttp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// ---------------------------------------------------------------- router

type dMatch struct {
	pattern string
	params  map[string]string
}

// dNewFuzzRouter registers fixed routes; the handler records the matched
// pattern and every declared parameter.
func dNewFuzzRouter(got *dMatch) *Router {
	r := &Router{cors: &CorsOptions{}}
	record := func(keys ...string) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			*got = dMatch{pattern: req.RoutePath(), params: map[string]string{}}
			for _, k := range keys {
				got.params[k] = req.Param(k)
			}
		}
	}
	r.GET("/", record())
	r.GET("/static/x", record())
	r.GET("/users/:id", record("id"))
	r.GET("/a/:x/b/:y", record("x", "y"))
	r.GET("/v/:n|^[0-9]+$", record("n"))
	r.GET("/files/...", record("..."))
	return r
}

var dDigitsRe = regexp.MustCompile(`^\d+$`)

// dExpectedRoute names the fixture route that an escaped path selects.
// It models the documented rules directly: one trailing slash is ignored, a
// :param needs a non-empty segment, "..." needs at least one segment (it may
// be empty), and a regex param must match the whole segment.
func dExpectedRoute(escaped string) string {
	segs := strings.Split(escaped, "/")
	if segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}

	n := len(segs)
	nonEmpty := func(i int) bool { return segs[i] != "" }

	switch {
	case n == 1 && segs[0] == "":
		return "/"
	case n == 3 && segs[1] == "static" && segs[2] == "x":
		return "/static/x"
	case n == 3 && segs[1] == "users" && nonEmpty(2):
		return "/users/:id"
	case n == 5 && segs[1] == "a" && segs[3] == "b" && nonEmpty(2) && nonEmpty(4):
		return "/a/:x/b/:y"
	case n == 3 && segs[1] == "v" && dDigitsRe.MatchString(segs[2]):
		return "/v/:n|^[0-9]+$"
	case n >= 3 && segs[1] == "files":
		return "/files/..."
	}

	return ""
}

func FuzzRouterMatch(f *testing.F) {
	for _, p := range []string{
		"", "/", "//", "/users/1", "/users/", "/users//", "/users", "/users/1/2",
		"/a/1/b/2", "/a//b/2", "/a/1/b/", "/v/123", "/v/12a", "/v/", "/files/a/b",
		"/files/", "/files", "/files/..", "/files/%2e%2e/x", "/users/%00", "/users/a b",
		"/users/\xff", "/static/x/", "/STATIC/x", "/users/%2e%2e", "/users/a%2Fb",
		"/files/%2F", "/files/a%2Fb/c", "/v/%30", "/users/%00", "/a/%2e/b/%2e%2e",
	} {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, path string) {
		var got dMatch
		r := dNewFuzzRouter(&got)

		// Parse the target as a server does, so RawPath is set and %2e%2e,
		// %2F and %00 reach the router as on the wire (RFC 9112 §3.2).
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")))
		if err != nil {
			t.Skip() // not a request target a server would accept
		}

		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req) // invariant 1: no panic

		// invariant 4: the matched route is the one that the segment rules
		// give (README routing), by an oracle independent of route.match.
		// A path with percent-encoding is the C10 domain: the router may decode
		// segments before it matches (RFC 3986 §2.1), so the oracle on the
		// escaped form does not apply. TestRouterParamDecoding covers it.
		if want := dExpectedRoute(req.URL.EscapedPath()); !strings.Contains(req.URL.EscapedPath(), "%") && got.pattern != want {
			t.Errorf("[README routing] %q routed to %q, want %q", req.URL.EscapedPath(), got.pattern, want)
		}

		if got.pattern == "" {
			return
		}

		// invariant 2: a matched parameter is always a segment of the path
		// the router saw (RFC 3986 §3.3), never invented or merged.
		// With a "%" in the path the value may be raw or decoded (C10, RFC 3986
		// §2.1, §2.2); only check the values when no "%" is present.
		escaped := req.URL.EscapedPath()
		if strings.Contains(escaped, "%") {
			return
		}

		segs := strings.Split(escaped, "/")
		for k, v := range got.params {
			if k == "..." {
				// the router drops one trailing slash before matching.
				if !strings.HasSuffix(strings.TrimSuffix(escaped, "/"), v) {
					t.Errorf("[RFC 3986 §3.3] wildcard %q is not a suffix of path %q", v, escaped)
				}
				continue
			}
			found := false
			for _, s := range segs {
				if s == v {
					found = true
				}
			}
			if v == "" || !found {
				t.Errorf("[RFC 3986 §3.3] param %s=%q is not a non-empty segment of %q (pattern %s)", k, v, escaped, got.pattern)
			}
		}

		// invariant 3: the anchored regex route only receives digits (S12).
		// The regex is "^[0-9]+$" in the fixture, so it is anchored by hand.
		if n, ok := got.params["n"]; ok && !regexp.MustCompile(`^\d+$`).MatchString(n) {
			t.Errorf("[S12] /v/:n got %q, not digits", n)
		}
	})
}

// ---------------------------------------------------------------- range

// dRangeSimple matches a valid single byte range: a-b, a- or -n (RFC 9110 §14.1.2).
var dRangeSimple = regexp.MustCompile(`^bytes=(?:(\d{1,9})-(\d{0,9})|-(\d{1,9}))$`)

// dRangeClass reports whether header is a valid single range for the oracle
// (a <= b, n > 0), and whether it is in the C2 domain, where fileParseRange
// differs from http.ServeContent today: open ended, suffix, or an end of 0.
func dRangeClass(header string) (valid, known bool) {
	m := dRangeSimple.FindStringSubmatch(header)
	if m == nil {
		return false, false
	}

	if m[3] != "" { // bytes=-n
		n, _ := strconv.ParseInt(m[3], 10, 64)
		return n > 0, true
	}

	if m[2] == "" { // bytes=a-
		return true, true
	}

	a, _ := strconv.ParseInt(m[1], 10, 64)
	b, _ := strconv.ParseInt(m[2], 10, 64)

	return a <= b, b == 0
}

// dServeContentRange is the oracle: what http.ServeContent answers for header
// on a body of size bytes. ok is false when it ignores the range (status 200).
func dServeContentRange(header string, size int64) (start, end int64, code int, ok bool) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Header.Set("Range", header)

	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "", time.Time{}, bytes.NewReader(make([]byte, size)))

	switch rec.Code {
	case http.StatusPartialContent:
		var total int64
		if _, err := fmt.Sscanf(rec.Header().Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
			return 0, 0, 0, false
		}

		return start, end, rec.Code, true
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, 0, rec.Code, true
	}

	return 0, 0, rec.Code, false
}

func FuzzParseRange(f *testing.F) {
	for _, h := range []string{
		"bytes=0-0", "bytes=0-", "bytes=-5", "bytes=5-2", "bytes=0-99", "bytes=9-9",
		"bytes=10-10", "bytes=-1-2", "bytes=0-1,3-4", "", "bytes=", "items=0-1",
		"bytes=9223372036854775807-9223372036854775807", "bytes=0-9223372036854775807",
		"bytes=+1-2", "bytes= 1-2", "bytes=1 -2", "bytes=00-01",
	} {
		f.Add(h, int64(10))
	}
	f.Add("bytes=2-5", int64(10))
	f.Add("bytes=0-99", int64(10))
	f.Add("bytes=0-0", int64(0))
	f.Add("bytes=0-0", int64(1))
	f.Add("bytes=0-5", int64(-1))

	f.Fuzz(func(t *testing.T, header string, size int64) {
		s, e, code := fileParseRange(header, size) // invariant 1: no panic

		// invariant 2 (RFC 9110 §14.1.2 / §15.5.17): 206 carries a satisfiable
		// range inside the representation; anything else is 416 with no range.
		switch code {
		case http.StatusPartialContent:
			if s < 0 || s > e || e >= size {
				t.Errorf("[RFC 9110 §14.1.2] %q size=%d: 206 with out-of-bounds range %d-%d", header, size, s, e)
			}
		case http.StatusRequestedRangeNotSatisfiable:
			if s != 0 || e != 0 {
				t.Errorf("[RFC 9110 §15.5.17] %q size=%d: 416 returned range %d-%d", header, size, s, e)
			}
		default:
			t.Errorf("status %d is neither 206 nor 416", code)
		}

		// invariant 3: for a valid single range outside the C2 domain, the
		// answer is exactly the one of http.ServeContent (RFC 9110 §14.1.2).
		// The oracle is unreliable for an empty body, so size starts at 1.
		valid, known := dRangeClass(header)
		if !valid || known || size < 1 || size > 1<<20 {
			return
		}

		ws, we, wcode, ok := dServeContentRange(header, size)
		if ok && [3]int64{s, e, int64(code)} != [3]int64{ws, we, int64(wcode)} {
			t.Errorf("[RFC 9110 §14.1.2] %q size=%d: got %d-%d/%d, ServeContent %d-%d/%d", header, size, s, e, code, ws, we, wcode)
		}
	})
}

// ---------------------------------------------------------------- accepts

// dTokenRe is a media-type token without regexp metacharacters.
var dTokenRe = regexp.MustCompile(`^[a-z0-9]+$`)

// dAcceptsKnown reports whether the input is in the C8 domain (Accept header
// used as an unanchored regex), where the behaviour is a known bug.
func dAcceptsKnown(accept, value string) bool {
	return strings.ContainsAny(accept+value, `*+.^$()[]{}|\?,;`) ||
		strings.Contains(strings.ToLower(value), strings.ToLower(accept)) ||
		strings.Contains(strings.ToLower(accept), strings.ToLower(value))
}

// dIsToken reports whether s is a token: one or more tchar (RFC 9110 §5.6.2).
func dIsToken(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}

		return false
	}

	return true
}

// dIsMediaRange reports whether m is "type/subtype", "type/*" or "*/*" with
// token parts (RFC 9110 §12.5.1), without parameters.
func dIsMediaRange(m string) bool {
	typ, sub, ok := strings.Cut(m, "/")
	if !ok {
		return false
	}

	if typ == "*" {
		return sub == "*"
	}

	return dIsToken(typ) && (sub == "*" || dIsToken(sub))
}

// dPlainWildcard reports whether accept is a list that has a "*/*" member, no
// ";" anywhere (so no q value), and only valid media ranges as members (empty
// members are ignored, RFC 9110 §5.6.1). An invalid member makes the field
// invalid, and C8 answers it with 406, so the invariant does not apply.
func dPlainWildcard(accept string) bool {
	if strings.Contains(accept, ";") {
		return false
	}

	wildcard := false

	for _, m := range strings.Split(accept, ",") {
		m = strings.Trim(m, " \t")
		if m == "" {
			continue
		}

		if !dIsMediaRange(m) {
			return false
		}

		wildcard = wildcard || m == "*/*"
	}

	return wildcard
}

func FuzzAccepts(f *testing.F) {
	for _, s := range [][2]string{
		{"", "text/html"}, {"*/*", "text/html"}, {"text/html", "text/html"},
		{"text/html", "application/json"}, {"text/*", "text/html"},
		{"application/json, text/plain", "text/plain"}, {"text/html;q=0", "text/html"},
		{"application/vnd.api+json", "application/vnd.api+json"},
		{"*/*,(", "text/html"}, {"*/*;q=0", "text/html"}, {"text/html;q=0, */*", "text/html"}, {"(", "text/html"}, {"text/html", "("}, {"text/html;q=", "text/html"},
	} {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, accept, value string) {
		req := &Request{Request: &http.Request{Header: http.Header{}}}
		if accept != "" {
			req.Request.Header.Set(HeaderAccept, accept)
		}

		got := req.Accepts(value) // invariant 1: no panic

		// invariant 2 (RFC 9110 §12.5.1): an empty Accept, or a list that has
		// a plain */* member and no parameters at all, accepts every media
		// type. A parameter can be q=0, which excludes the type (§12.4.2), so
		// "*/*;q=0" and "text/html;q=0, */*" are outside this invariant.
		if (accept == "" || dPlainWildcard(accept)) && !got {
			t.Errorf("[RFC 9110 §12.5.1] Accept %q must accept %q", accept, value)
		}

		// invariant 3: two distinct plain "type/subtype" tokens never match
		// (RFC 9110 §8.3.1). Outside the C8 domain only.
		at, as, aok := strings.Cut(accept, "/")
		vt, vs, vok := strings.Cut(value, "/")
		if aok && vok && dTokenRe.MatchString(at) && dTokenRe.MatchString(as) &&
			dTokenRe.MatchString(vt) && dTokenRe.MatchString(vs) &&
			!dAcceptsKnown(accept, value) && got {
			t.Errorf("[RFC 9110 §8.3.1] Accept %q matched different type %q", accept, value)
		}
	})
}

// ---------------------------------------------------------------- vary

func dVaryTokens(h string) []string {
	var out []string
	for _, p := range strings.Split(h, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

func FuzzVary(f *testing.F) {
	for _, s := range [][2]string{
		{"Origin", "Accept"}, {"Accept", "Accept"}, {"Accept", "accept"},
		{"Accept-Encoding", "Origin"}, {"A", "B"}, {"", "Origin"}, {"Origin", ""},
	} {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		if a == "" || b == "" || strings.ContainsAny(a+b, ",") || strings.TrimSpace(a) != a || strings.TrimSpace(b) != b {
			t.Skip()
		}

		rec := httptest.NewRecorder()
		res := &ResponseWriter{w: rec, request: httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)}
		res.Vary(a)
		res.Vary(b)
		res.Vary(a)

		// invariant (RFC 9110 §12.5.5): every requested name is listed, once.
		tokens := dVaryTokens(rec.Header().Get(HeaderVary))
		count := map[string]int{}
		for _, tk := range tokens {
			count[tk]++
		}
		for _, n := range []string{a, b} {
			if count[strings.ToLower(n)] != 1 {
				t.Errorf("[RFC 9110 §12.5.5] Vary %q: %q appears %d times", rec.Header().Get(HeaderVary), n, count[strings.ToLower(n)])
			}
		}
	})
}

// ---------------------------------------------------------------- cors

func FuzzCorsOrigin(f *testing.F) {
	for _, s := range [][4]string{
		{"http://a.com", "", "a.com", "http://a.com"},
		{"http://a.com", "cross-site", "a.com", "http://a.com"},
		{"http://evil.com", "cross-site", "a.com", "http://a.com"},
		{"null", "cross-site", "a.com", "null"},
		{"", "cross-site", "a.com", "http://a.com"},
		{"http://a.com", "same-origin", "b.com", "http://a.com"},
		{"http://a.com", "same-site", "b.com", "*"},
		{"%zz", "", "a.com", "http://a.com"},
		{"http://a.com", "", "a.com", ""},
	} {
		f.Add(s[0], s[1], s[2], s[3])
	}

	f.Fuzz(func(t *testing.T, origin, site, host, allowed string) {
		req := &Request{Request: &http.Request{Header: http.Header{}, Host: host}}
		req.Request.Header.Set(HeaderOrigin, origin)
		req.Request.Header.Set(HeaderSecFetchSite, site)
		opts := &CorsOptions{Origin: []string{allowed}}

		got := corsAcceptOrigin(req, opts) // invariant 1: no panic

		// With an empty Sec-Fetch-Site production fails open (S3), so that
		// domain has no oracle here; the KnownBug rows below cover it.
		switch {
		case allowed == "*":
			ehttptest.True(t, "Fetch §3.2.3", got, "* allowlist must accept %q", origin)
		case site == "same-origin" || site == "none":
			ehttptest.True(t, "Fetch Metadata §2.1", got, "Sec-Fetch-Site %q is not a cross-origin request", site)
		case site == "cross-site" || site == "same-site" || (site != "" && site != "same-origin" && site != "none"):
			// invariant: a cross-origin request passes iff its origin equals the
			// allowlist entry exactly (case-sensitive, no normalisation). The
			// oracle is a plain string compare, not production's corsMatch.
			ehttptest.Eq(t, "Fetch §3.2.5", got, origin != "" && origin == allowed)
		}
	})
}

// ------------------------------------------------- known bugs found/fed by C5
//
// Each test reproduces, with a concrete input, a bug that a fuzz target above
// skips on purpose. They run in the normal `go test` as SKIP with the ID.

func TestFuzzKnownBugs(t *testing.T) {
	t.Run("S11 Vary compares by substring", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		res := &ResponseWriter{w: rec, request: httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)}
		res.Vary("Accept-Encoding")
		res.Vary("Accept")
		ehttptest.Eq(t, "RFC 9110 §12.5.5", rec.Header().Get(HeaderVary), "Accept-Encoding, Accept")
	})

	t.Run("C8 Accepts matches substring of other type", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C8", func(t testing.TB) {
			req := &Request{Request: &http.Request{Header: http.Header{HeaderAccept: {"text/html"}}}}
			ehttptest.Eq(t, "RFC 9110 §8.3.1", req.Accepts("text/htmlx"), false)
		})
	})

	t.Run("C8 Accepts ignores a list", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C8", func(t testing.TB) {
			req := &Request{Request: &http.Request{Header: http.Header{HeaderAccept: {"application/json, text/plain"}}}}
			ehttptest.Eq(t, "RFC 9110 §12.5.1", req.Accepts("text/plain"), true)
		})
	})

	t.Run("C8 Accepts treats q=0 as acceptable", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C8", func(t testing.TB) {
			req := &Request{Request: &http.Request{Header: http.Header{HeaderAccept: {"text/html;q=0"}}}}
			ehttptest.Eq(t, "RFC 9110 §12.5.1", req.Accepts("text/html"), false)
		})
	})

	rangeCases := []struct {
		name, header string
		size         int64
		s, e         int64
		code         int
	}{
		{"bytes=0-0 is the first byte", "bytes=0-0", 10, 0, 0, http.StatusPartialContent},
		{"bytes=N- is open ended", "bytes=5-", 10, 5, 9, http.StatusPartialContent},
		{"bytes=-N is a suffix", "bytes=-3", 10, 7, 9, http.StatusPartialContent},
	}
	for _, tc := range rangeCases {
		t.Run("C2 "+tc.name, func(t *testing.T) {
			t.Parallel()
			ehttptest.KnownBug(t, "C2", func(t testing.TB) {
				s, e, code := fileParseRange(tc.header, tc.size)
				ehttptest.Eq(t, "RFC 9110 §14.1.2", [3]int64{s, e, int64(code)}, [3]int64{tc.s, tc.e, int64(tc.code)})
			})
		})
	}

	t.Run("C2 fileParseRange equals http.ServeContent", func(t *testing.T) {
		t.Parallel()

		headers := []string{
			"bytes=0-0", "bytes=2-5", "bytes=0-99", "bytes=9-9", "bytes=10-10", "bytes=0-9",
			"bytes=0-", "bytes=5-", "bytes=9-", "bytes=-1", "bytes=-3", "bytes=-99",
		}

		for _, h := range headers {
			valid, known := dRangeClass(h)
			if !valid {
				t.Fatalf("test header %q is not a valid single range", h)
			}

			check := func(t testing.TB) {
				ws, we, wcode, ok := dServeContentRange(h, 10)
				if !ok {
					t.Fatalf("oracle ignored %q", h)
				}

				s, e, code := fileParseRange(h, 10)
				ehttptest.Eq(t, "RFC 9110 §14.1.2 (oracle: http.ServeContent) "+h, [3]int64{s, e, int64(code)}, [3]int64{ws, we, int64(wcode)})
			}

			t.Run(h, func(t *testing.T) {
				if known {
					ehttptest.KnownBug(t, "C2", check)
					return
				}

				check(t)
			})
		}
	})

	const inmEtag = `W/"1-00000001"`
	for _, inm := range []string{"*", `"x", ` + inmEtag, strings.TrimPrefix(inmEtag, "W/")} {
		t.Run("C3 If-None-Match "+inm, func(t *testing.T) {
			t.Parallel()
			ehttptest.KnownBug(t, "C3", func(t testing.TB) {
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
				r.Header.Set(HeaderIfNoneMatch, inm)
				res := &ResponseWriter{request: r}
				ehttptest.Eq(t, "RFC 9110 §13.1.2", fileIsNotModified(res, inmEtag, time.Unix(100, 0)), true)
			})
		})
	}

	t.Run("C2 If-None-Match wins over If-Modified-Since", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C2", func(t testing.TB) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.Header.Set(HeaderIfNoneMatch, `W/"other"`)
			r.Header.Set(HeaderIfModifiedSince, time.Unix(200, 0).UTC().Format(http.TimeFormat))
			res := &ResponseWriter{request: r}
			got := fileIsNotModified(res, `W/"1-00000001"`, time.Unix(100, 0))
			ehttptest.Eq(t, "RFC 9110 §13.1.3", got, false)
		})
	})

	t.Run("C2 If-Modified-Since never matches a sub-second mtime", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C2", func(t testing.TB) {
			mod := time.Unix(100, 500_000_000) // real file systems keep ns; Last-Modified is truncated to s
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.Header.Set(HeaderIfModifiedSince, time.Unix(100, 0).UTC().Format(http.TimeFormat))
			res := &ResponseWriter{request: r}
			ehttptest.Eq(t, "RFC 9110 §13.1.3", fileIsNotModified(res, `W/"e"`, mod), true)
		})
	})

	t.Run("C12 params leak from a route that did not match", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "C12", func(t testing.TB) {
			var seen string
			r := &Router{cors: &CorsOptions{}}
			r.GET("/u/:a/x", func(*ResponseWriter, *Request) {})
			r.GET("/u/:b/y", func(_ *ResponseWriter, req *Request) { seen = req.Param("a") })
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/5/y", http.NoBody)
			ehttptest.Do(r, req)
			ehttptest.Eq(t, "C12", seen, "")
		})
	})

	t.Run("S12 route regex is not anchored", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "S12", func(t testing.TB) {
			status := 0
			r := &Router{cors: &CorsOptions{}}
			r.GET("/i/:id|[0-9]+", func(res *ResponseWriter, _ *Request) { res.SendStatus(200) })
			status = ehttptest.Do(r, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/i/abc1", http.NoBody)).Code
			ehttptest.Eq(t, "S12", status, 404)
		})
	})

	t.Run("S12 invalid route regex panics at request time", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "S12", func(t testing.TB) {
			defer func() { _ = recover() }()
			r := &Router{cors: &CorsOptions{}}
			r.GET("/i/:id|[", func(*ResponseWriter, *Request) {}) // must panic here (boot)
			ehttptest.True(t, "S12", false, "Handle accepted an invalid regex; it panics later, per request")
		})
	})

	t.Run("S3 CORS foreign origin accepted without Sec-Fetch-Site", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			// The origin host equals the Host header, but the origin is not on
			// the allowlist: S3 allows an origin iff it is in the allowlist.
			req := &Request{Request: &http.Request{Header: http.Header{}, Host: "evil.example"}}
			req.Request.Header.Set(HeaderOrigin, "http://evil.example")
			opts := &CorsOptions{Origin: []string{"http://app.example"}}
			ehttptest.Eq(t, "Fetch §3.2.5 + S3 (allowed iff in the allowlist)", corsAcceptOrigin(req, opts), false)
		})
	})

	t.Run("S3 (ND-1) CORS allowlist ignored without Sec-Fetch-Site", func(t *testing.T) {
		t.Parallel()
		ehttptest.KnownBug(t, "S3", func(t testing.TB) {
			req := &Request{Request: &http.Request{Header: http.Header{}, Host: "api.example"}}
			req.Request.Header.Set(HeaderOrigin, "http://app.example")
			opts := &CorsOptions{Origin: []string{"http://app.example"}}
			ehttptest.Eq(t, "Fetch §3.2.5", corsAcceptOrigin(req, opts), true)
		})
	})
}
