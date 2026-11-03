// Fuzzing with invariants. Package ehttp (internal) so the targets can
// reach route.match, SendFile, corsAcceptOrigin.
//
// How known bugs are handled: a fuzz target cannot be an expected failure.
// When fuzzing finds a bug that already has a name, the crasher stays in
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
	"net/url"
	"os"
	"path/filepath"
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
	r := &Router{}
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
	r.GET("/v/:n|[0-9]+", record("n"))
	r.GET("/w/:m|a|b", record("m"))
	r.GET("/files/...", record("..."))
	return r
}

// onlySlashEscapes reports whether every "%" of escaped starts %2F or %2f.
func onlySlashEscapes(escaped string) bool {
	rest := escaped

	for {
		_, after, found := strings.Cut(rest, "%")
		if !found {
			return true
		}

		if len(after) < 2 || after[0] != '2' || (after[1] != 'F' && after[1] != 'f') {
			return false
		}

		rest = after[2:]
	}
}

// dCheckDecodedParams checks the params of a path with percent escapes: each
// one is the url.PathUnescape of its raw segment (RFC 3986 §2.1), and the
// wildcard is the decoded rest of the path.
func dCheckDecodedParams(t *testing.T, escaped string, params map[string]string) {
	t.Helper()

	for k, v := range params {
		if k == "..." {
			whole, err := url.PathUnescape(strings.TrimSuffix(escaped, "/"))
			if err == nil && !strings.HasSuffix(whole, v) {
				t.Errorf("[RFC 3986 §2.1] wildcard %q is not a decoded suffix of %q", v, escaped)
			}

			continue
		}

		found := false

		for _, seg := range strings.Split(escaped, "/") {
			if dec, err := url.PathUnescape(seg); err == nil && dec == v {
				found = true
			}
		}

		if !found {
			t.Errorf("[RFC 3986 §2.1] param %s=%q is not the decoded form of a segment of %q", k, v, escaped)
		}
	}
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
		return "/v/:n|[0-9]+"
	case n == 3 && segs[1] == "w" && (segs[2] == "a" || segs[2] == "b"):
		return "/w/:m|a|b"
	case n >= 3 && segs[1] == "files":
		return "/files/..."
	}

	return ""
}

func FuzzRouterMatch(f *testing.F) {
	for _, p := range []string{
		"", "/", "//", "/users/1", "/users/", "/users//", "/users", "/users/1/2",
		"/a/1/b/2", "/a//b/2", "/a/1/b/", "/v/123", "/v/12a", "/v/", "/w/a", "/w/b", "/w/ab", "/w/c", "/files/a/b",
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

		// A refused value (403 for "..", 400 for NUL or backslash) never reaches
		// the handler. Only a path with "..", "%" or a backslash can be refused.
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusBadRequest {
			if e := req.URL.EscapedPath(); !strings.ContainsAny(e, "%\\") && !strings.Contains(e, "..") {
				t.Errorf("[RFC 9110 §15.5] %q was refused with %d", e, rec.Code)
			}

			if got.pattern != "" {
				t.Errorf("[RFC 9110 §15.5] %q was refused but the handler ran", req.URL.EscapedPath())
			}

			return
		}

		// invariant 4: the matched route is the one that the segment rules
		// give (README routing), by an oracle independent of route.match.
		// A path with percent-encoding is the percent-decoding domain: the router may decode
		// segments before it matches (RFC 3986 §2.1), so the oracle on the
		// escaped form does not apply. TestRouterParamDecoding covers it.
		// A decoded "/" must not change the route: when %2F is the only escape,
		// the oracle on the escaped form still applies. A refused value (403,
		// 400) never reaches the handler, so it records no match.
		oracleApplies := !strings.Contains(req.URL.EscapedPath(), "%") || onlySlashEscapes(req.URL.EscapedPath())
		if want := dExpectedRoute(req.URL.EscapedPath()); oracleApplies && got.pattern != want {
			t.Errorf("[README routing] %q routed to %q, want %q", req.URL.EscapedPath(), got.pattern, want)
		}

		if got.pattern == "" {
			return
		}

		// invariant 2: a matched parameter is always a segment of the path
		// the router saw (RFC 3986 §3.3), never invented or merged.
		// With a "%" in the path the value may be raw or decoded (RFC 3986
		// §2.1, §2.2); only check the values when no "%" is present.
		escaped := req.URL.EscapedPath()
		if strings.Contains(escaped, "%") {
			dCheckDecodedParams(t, escaped, got.params)
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

		// invariant 3: the anchored regex route only receives digits.
		// The fixture regexes are not anchored by hand: the router anchors them.
		if n, ok := got.params["n"]; ok && !regexp.MustCompile(`^\d+$`).MatchString(n) {
			t.Errorf("/v/:n got %q, not digits", n)
		}

		if m, ok := got.params["m"]; ok && m != "a" && m != "b" {
			t.Errorf("/w/:m got %q, not a or b", m)
		}
	})
}

// ---------------------------------------------------------------- range

// dRangeSimple matches a valid single byte range: a-b, a- or -n (RFC 9110 §14.1.2).
var dRangeSimple = regexp.MustCompile(`^bytes=(?:(\d{1,9})-(\d{0,9})|-(\d{1,9}))$`)

// dRangeValid reports whether header is a valid single range for the oracle
// (a <= b, n > 0).
func dRangeValid(header string) bool {
	m := dRangeSimple.FindStringSubmatch(header)
	if m == nil {
		return false
	}

	if m[3] != "" { // bytes=-n
		n, _ := strconv.ParseInt(m[3], 10, 64)
		return n > 0
	}

	if m[2] == "" { // bytes=a-
		return true
	}

	a, _ := strconv.ParseInt(m[1], 10, 64)
	b, _ := strconv.ParseInt(m[2], 10, 64)

	return a <= b
}

// dRangeBody is the body of the file that the range targets serve.
const dRangeBody = "0123456789"

// dRangeFile writes a file of dRangeBody and returns its path.
func dRangeFile(tb testing.TB) string {
	tb.Helper()

	name := filepath.Join(tb.TempDir(), "f.txt")
	if err := os.WriteFile(name, []byte(dRangeBody), 0o600); err != nil {
		tb.Fatal(err)
	}

	return name
}

// dRangeServer serves a file of dRangeBody at /f, and at /missing with the
// status 404 set before SendFile.
func dRangeServer(tb testing.TB) *Server {
	tb.Helper()

	name := dRangeFile(tb)

	s := NewServer()
	s.GET("/f", func(res *ResponseWriter, _ *Request) { res.SendFile(name) })
	s.GET("/missing", func(res *ResponseWriter, _ *Request) { res.Status(StatusNotFound); res.SendFile(name) })

	return s
}

// dSend asks path of s and returns the answer. kv are header names and values.
func dSend(s *Server, method, path string, kv ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, http.NoBody)

	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	return rec
}

// dSendFile asks /f of s with a Range header and returns the answer.
func dSendFile(s *Server, header string) *httptest.ResponseRecorder {
	return dSend(s, http.MethodGet, "/f", HeaderRange, header)
}

// dBlankSet reports that header is a bytes range with no member but commas and spaces.
func dBlankSet(header string) bool {
	set, ok := strings.CutPrefix(header, "bytes=")
	return ok && strings.Trim(set, ", ") == ""
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

// dUnknownUnit reports that header is a token and "=", and the token is not "bytes".
func dUnknownUnit(header string) bool {
	unit, _, ok := strings.Cut(header, "=")
	return ok && isToken(unit) && !strings.EqualFold(unit, "bytes")
}

func FuzzParseRange(f *testing.F) {
	for _, h := range []string{
		"bytes=0-0", "bytes=0-", "bytes=-5", "bytes=5-2", "bytes=0-99", "bytes=9-9",
		"bytes=10-10", "bytes=-1-2", "bytes=0-1,3-4", "", "bytes=", "items=0-1",
		"bytes=9223372036854775807-9223372036854775807", "bytes=0-9223372036854775807",
		"bytes=+1-2", "bytes= 1-2", "bytes=1 -2", "bytes=00-01", "Bytes=0-1", "bytes=2-5",
		"bytes=,", "bytes= ", "bytes=-0", "bytes=-0,1-2", "bytes=0-1,-0", "bytes=-0,-0",
	} {
		f.Add(h, false)
		f.Add(h, true)
	}

	s := dRangeServer(f)

	f.Fuzz(func(t *testing.T, header string, head bool) {
		if header == "" || !validFieldValue(header) {
			return
		}

		// invariant 0: a status set before SendFile is the status, with the
		// full body: Range is not used (RFC 9110 §14.2).
		missing := dSend(s, http.MethodGet, "/missing", HeaderRange, header, "If-None-Match", "*")
		ehttptest.Eq(t, "RFC 9110 §13.2.1 "+header, missing.Code, http.StatusNotFound)
		ehttptest.Eq(t, "RFC 9110 §14.2 "+header, missing.Body.String(), dRangeBody)

		// invariant 5: Range is defined for GET only (RFC 9110 §14.2).
		if head {
			rec := dSend(s, http.MethodHead, "/f", HeaderRange, header)
			ehttptest.Eq(t, "RFC 9110 §14.2 "+header, rec.Code, http.StatusOK)
			ehttptest.Eq(t, "RFC 9110 §9.3.2 "+header, rec.Header().Get(HeaderContentLength), "10")
			ehttptest.Eq(t, "RFC 9110 §9.3.2 "+header, rec.Body.Len(), 0)

			return
		}

		rec := dSendFile(s, header) // invariant 1: no panic

		// invariant 4: a set with no member is not valid (RFC 9110 §14.1.1).
		if dBlankSet(header) {
			ehttptest.Eq(t, "RFC 9110 §14.1.1 "+header, rec.Code, http.StatusRequestedRangeNotSatisfiable)
		}

		// invariant 2 (RFC 9110 §14.2, §15.5.17): the answer is 200, 206 or 416.
		switch rec.Code {
		case http.StatusOK:
			ehttptest.Eq(t, "RFC 9110 §14.2 "+header, rec.Body.String(), dRangeBody)
		case http.StatusPartialContent:
			checkPartial(t, header, rec)
		case http.StatusRequestedRangeNotSatisfiable:
			ehttptest.Eq(t, "RFC 9110 §15.5.17 "+header, rec.Header().Get(HeaderContentRange), "bytes */10")
		default:
			t.Errorf("%q: status %d is not 200, 206 or 416", header, rec.Code)
		}

		// invariant 3: a unit that is not "bytes" is ignored (RFC 9110 §14.2).
		if dUnknownUnit(header) {
			ehttptest.Eq(t, "RFC 9110 §14.2 "+header, rec.Code, http.StatusOK)
		}

		// invariant 6: a valid single range gets the answer of http.ServeContent.
		if !dRangeValid(header) {
			return
		}

		ws, we, wcode, ok := dServeContentRange(header, int64(len(dRangeBody)))
		if !ok {
			return
		}

		want := fmt.Sprintf("bytes %d-%d/10", ws, we)
		if wcode == http.StatusRequestedRangeNotSatisfiable {
			want = "bytes */10"
		}

		if rec.Code != wcode || rec.Header().Get(HeaderContentRange) != want {
			t.Errorf("[RFC 9110 §14.1.2] %q: got %d %q, ServeContent %d %q", header, rec.Code, rec.Header().Get(HeaderContentRange), wcode, want)
		}
	})
}

// checkPartial checks a 206: one range inside the file with its bytes, or many.
func checkPartial(t *testing.T, header string, rec *httptest.ResponseRecorder) {
	t.Helper()

	if strings.HasPrefix(rec.Header().Get(HeaderContentType), "multipart/byteranges") {
		return
	}

	var s, e, total int64
	if _, err := fmt.Sscanf(rec.Header().Get(HeaderContentRange), "bytes %d-%d/%d", &s, &e, &total); err != nil {
		t.Errorf("[RFC 9110 §14.4] %q: 206 without a valid Content-Range: %v", header, err)
		return
	}

	if s < 0 || s > e || e >= total || total != int64(len(dRangeBody)) {
		t.Errorf("[RFC 9110 §14.1.2] %q: 206 with out-of-bounds range %d-%d/%d", header, s, e, total)
		return
	}

	ehttptest.Eq(t, "RFC 9110 §14.4 "+header, rec.Body.String(), dRangeBody[s:e+1])
}

// ---------------------------------------------------------------- accepts

// dTokenRe is a media-type token without regexp metacharacters.
var dTokenRe = regexp.MustCompile(`^[a-z0-9]+$`)

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
// invalid, and Accepts answers it with 406, so the invariant does not apply.
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

		// invariant 2 (RFC 9110 §12.5.1): for a valid value, an empty Accept, or a list that has
		// a plain */* member and no parameters at all, accepts every media
		// type. A parameter can be q=0, which excludes the type (§12.4.2), so
		// "*/*;q=0" and "text/html;q=0, */*" are outside this invariant.
		vt0, vs0, vok0 := strings.Cut(value, "/")
		if vok0 && dIsToken(vt0) && dIsToken(vs0) && (accept == "" || dPlainWildcard(accept)) && !got {
			t.Errorf("[RFC 9110 §12.5.1] Accept %q must accept %q", accept, value)
		}

		// invariant 3: two distinct plain "type/subtype" tokens never match
		// (RFC 9110 §8.3.1).
		at, as, aok := strings.Cut(accept, "/")
		vt, vs, vok := strings.Cut(value, "/")
		if aok && vok && dTokenRe.MatchString(at) && dTokenRe.MatchString(as) &&
			dTokenRe.MatchString(vt) && dTokenRe.MatchString(vs) &&
			!strings.EqualFold(accept, value) && got {
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
		{"0", "*"}, {"*", "Origin"}, // run 41611842708: "*" covers every name
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

		// invariant (RFC 9110 §12.5.5): "*" covers every other name, so the
		// field is exactly "*". Else every requested name is listed, once.
		if a == "*" || b == "*" {
			ehttptest.Eq(t, "RFC 9110 §12.5.5", rec.Header().Get(HeaderVary), "*")

			return
		}

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
	for _, s := range [][2]string{
		{"http://a.com", "http://a.com"},
		{"http://evil.com", "http://a.com"},
		{"null", "null"},
		{"", "http://a.com"},
		{"http://a.com", "*"},
		{"%zz", "http://a.com"},
		{"http://a.com", ""},
	} {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, origin, allowed string) {
		req := &Request{Request: &http.Request{Header: http.Header{}}}
		req.Request.Header.Set(HeaderOrigin, origin)
		opts := &CorsOptions{Origin: []string{allowed}}

		got := corsAcceptOrigin(req, opts) // invariant 1: no panic

		// Oracle: acceptance depends only on the origin (Fetch §3.2.5). A "*"
		// allowlist accepts all. Else the origin must equal the entry exactly
		// (case-sensitive, no normalisation) and must not be empty.
		want := allowed == "*" || (origin != "" && origin == allowed)
		ehttptest.Eq(t, "Fetch §3.2.5", got, want)
	})
}

// SendFile answers a Range through http.ServeContent (RFC 9110 §14).
func TestSendFileRange(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	tests := []struct {
		name, header string
		code         int
		contentRange string
		body         string
	}{
		{"bytes=0-0 is the first byte", "bytes=0-0", 206, "bytes 0-0/10", "0"},
		{"bytes=N- is open ended", "bytes=5-", 206, "bytes 5-9/10", "56789"},
		{"bytes=-N is a suffix", "bytes=-3", 206, "bytes 7-9/10", "789"},
		{"first-pos beyond the size", "bytes=10-12", 416, "bytes */10", ""},
		{"invalid syntax", "bytes=abc", 416, "bytes */10", ""},
		{"last-pos before first-pos", "bytes=5-2", 416, "bytes */10", ""},
		{"suffix of no byte", "bytes=-0", 416, "bytes */10", ""},
		{"unit is not case-sensitive", "Bytes=0-1", 206, "bytes 0-1/10", "01"},
		{"unknown unit is ignored", "items=0-1", 200, "", dRangeBody},
		{"no unit is invalid", "0-1", 416, "bytes */10", ""},
		{"empty set", "bytes=", 416, "bytes */10", ""},
		{"set of commas", "bytes=,", 416, "bytes */10", ""},
		{"set of a space", "bytes= ", 416, "bytes */10", ""},
		{"set of suffixes of no byte", "bytes=-0,-0", 416, "bytes */10", ""},
		{"suffix of no byte is dropped", "bytes=-0,1-2", 206, "bytes 1-2/10", "12"},
		{"suffix of no byte is dropped at the end", "bytes=1-2,-0", 206, "bytes 1-2/10", "12"},
		{"double minus is invalid", "bytes=0-1,--0", 416, "bytes */10", ""},
		{"minus plus is invalid", "bytes=0-1,-+0", 416, "bytes */10", ""},
		{"plus in a number is invalid", "bytes=+1-2", 416, "bytes */10", ""},
		{"empty member is skipped", "bytes=1-2,", 206, "bytes 1-2/10", "12"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := dSendFile(s, tt.header)

			ehttptest.Eq(t, "RFC 9110 §14", rec.Code, tt.code)
			ehttptest.Eq(t, "RFC 9110 §14.4", rec.Header().Get(HeaderContentRange), tt.contentRange)

			if tt.code != http.StatusRequestedRangeNotSatisfiable {
				ehttptest.Eq(t, "RFC 9110 §14.4", rec.Body.String(), tt.body)
			}
		})
	}
}

// ------------------------------------------------- known bugs found by fuzzing
//
// Each test reproduces, with a concrete input, a bug that a fuzz target above
// skips on purpose. They run in the normal `go test` as SKIP with the bug name.

func TestFuzzKnownBugs(t *testing.T) {
	t.Run("Vary compares by substring", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		res := &ResponseWriter{w: rec, request: httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)}
		res.Vary("Accept-Encoding")
		res.Vary("Accept")
		ehttptest.Eq(t, "RFC 9110 §12.5.5", rec.Header().Get(HeaderVary), "Accept-Encoding, Accept")
	})

	t.Run("params leak from a route that did not match", func(t *testing.T) {
		t.Parallel()
		var seen string
		r := &Router{}
		r.GET("/u/:a/x", func(*ResponseWriter, *Request) {})
		r.GET("/u/:b/y", func(_ *ResponseWriter, req *Request) { seen = req.Param("a") })
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/5/y", http.NoBody)
		ehttptest.Do(r, req)
		ehttptest.Eq(t, "params come only from the matched route", seen, "")
	})

	t.Run("route regex is not anchored", func(t *testing.T) {
		t.Parallel()
		status := 0
		r := &Router{}
		r.GET("/i/:id|[0-9]+", func(res *ResponseWriter, _ *Request) { res.SendStatus(200) })
		status = ehttptest.Do(r, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/i/abc1", http.NoBody)).Code
		ehttptest.Eq(t, "a regex param matches the whole segment", status, 404)
	})

	t.Run("unbalanced paren does not escape the anchor group", func(t *testing.T) {
		t.Parallel()
		defer func() { _ = recover() }()
		r := &Router{}
		r.GET("/i/:id|[0-9]+)|(x", func(*ResponseWriter, *Request) {}) // must panic here (boot)
		ehttptest.True(t, "an unbalanced regex is rejected", false, "Handle accepted an unbalanced regex")
	})

	t.Run("invalid route regex panics at registration", func(t *testing.T) {
		t.Parallel()
		defer func() { _ = recover() }()
		r := &Router{}
		r.GET("/i/:id|[", func(*ResponseWriter, *Request) {}) // must panic here (boot)
		ehttptest.True(t, "an invalid regex panics at registration", false, "Handle accepted an invalid regex; it panics later, per request")
	})

	t.Run("CORS foreign origin accepted without Sec-Fetch-Site", func(t *testing.T) {
		t.Parallel()
		// The origin host equals the Host header, but the origin is not on
		// the allowlist: an origin is allowed iff it is in the allowlist.
		req := &Request{Request: &http.Request{Header: http.Header{}, Host: "evil.example"}}
		req.Request.Header.Set(HeaderOrigin, "http://evil.example")
		opts := &CorsOptions{Origin: []string{"http://app.example"}}
		ehttptest.Eq(t, "Fetch §3.2.5 (allowed iff in the allowlist)", corsAcceptOrigin(req, opts), false)
	})

	t.Run("CORS allowlist ignored without Sec-Fetch-Site", func(t *testing.T) {
		t.Parallel()
		req := &Request{Request: &http.Request{Header: http.Header{}, Host: "api.example"}}
		req.Request.Header.Set(HeaderOrigin, "http://app.example")
		opts := &CorsOptions{Origin: []string{"http://app.example"}}
		ehttptest.Eq(t, "Fetch §3.2.5", corsAcceptOrigin(req, opts), true)
	})
}

// Range is defined for GET only (RFC 9110 §14.2): HEAD answers 200 with the
// real length and no body.
func TestSendFileHeadIgnoresRange(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	rec := dSend(s, http.MethodHead, "/f", HeaderRange, "bytes=0-0", "If-Range", "*")

	ehttptest.Eq(t, "RFC 9110 §14.2", rec.Code, 200)
	ehttptest.Eq(t, "RFC 9110 §14.2", rec.Header().Get(HeaderContentRange), "")
	ehttptest.Eq(t, "RFC 9110 §9.3.2", rec.Header().Get(HeaderContentLength), "10")
	ehttptest.Eq(t, "RFC 9110 §9.3.2", rec.Body.Len(), 0)
}

// A status set before SendFile is the status. Range and the conditional
// fields apply to a 2xx response only (RFC 9110 §13.2.1, §14.2).
func TestSendFileStatusBefore(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)

	t.Run("GET gets the full body", func(t *testing.T) {
		t.Parallel()

		rec := dSend(s, http.MethodGet, "/missing", HeaderRange, "bytes=0-1", HeaderIfNoneMatch, "*", "If-Match", `"nope"`)

		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 404)
		ehttptest.Eq(t, "RFC 9110 §14.2", rec.Body.String(), dRangeBody)
		ehttptest.Eq(t, "RFC 9110 §8.6", rec.Header().Get(HeaderContentLength), "10")
		ehttptest.True(t, "RFC 9110 §8.3", rec.Header().Get(HeaderContentType) != "", "no Content-Type")
		ehttptest.True(t, "RFC 9110 §8.8.2", rec.Header().Get(HeaderLastModified) != "", "no Last-Modified")
	})

	t.Run("204 and 304 have no content", func(t *testing.T) {
		t.Parallel()

		for _, code := range []int{204, 304} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
			res := &ResponseWriter{w: rec, request: req}
			res.Status(code)
			res.SendFile(dRangeFile(t))

			ehttptest.Eq(t, "RFC 9110 §15.3.5", rec.Code, code)
			ehttptest.Eq(t, "RFC 9110 §15.3.5", rec.Body.Len(), 0)
			ehttptest.Eq(t, "RFC 9110 §8.6", rec.Header().Get(HeaderContentLength), "")
			ehttptest.Eq(t, "RFC 9110 §8.3", rec.Header().Get(HeaderContentType), "")
		}
	})

	t.Run("HEAD has no body", func(t *testing.T) {
		t.Parallel()

		rec := dSend(s, http.MethodHead, "/missing")

		ehttptest.Eq(t, "RFC 9110 §9.3.2", rec.Code, 404)
		ehttptest.Eq(t, "RFC 9110 §9.3.2", rec.Header().Get(HeaderContentLength), "10")
		ehttptest.Eq(t, "RFC 9110 §9.3.2", rec.Body.Len(), 0)
	})
}

// An If-Match or If-None-Match with an invalid syntax is 400 (RFC 9110 §13.1.1, §13.1.2).
func TestSendFileInvalidEtagFields(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	tests := []struct{ name, field, value string }{
		{"If-Match unquoted", "If-Match", "abc"},
		{"If-None-Match star with a tag", HeaderIfNoneMatch, `*, "a"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := dSend(s, http.MethodGet, "/f", tt.field, tt.value)
			ehttptest.Eq(t, "RFC 9110 §15.5.1", rec.Code, 400)
		})
	}

	// Each field line counts.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/f", http.NoBody)
	req.Header.Add(HeaderIfNoneMatch, `"a"`)
	req.Header.Add(HeaderIfNoneMatch, "abc")

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	ehttptest.Eq(t, "RFC 9110 §15.5.1", rec.Code, 400)
}

// A Range that cannot be satisfied is answered after the preconditions (RFC
// 9110 §13.2.2: Range is step 5, and §13.1.5: a false If-Range ignores the
// Range). SendFile gives such a Range to http.ServeContent as an invalid
// value, so these tests guard the dependency on its parseRange.
func TestSendFileUnsatisfiableRangeAfterPreconditions(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	past := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	future := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)

	conditions := []struct {
		name     string
		kv       []string
		code     int
		body     string
		hasRange bool
	}{
		{"no conditional", nil, 416, "", true},
		{"If-Match fails", []string{"If-Match", `"nope"`}, 412, "", false},
		{"If-Unmodified-Since in the past", []string{"If-Unmodified-Since", past}, 412, "", false},
		{"If-None-Match star", []string{HeaderIfNoneMatch, "*"}, 304, "", false},
		{"If-Modified-Since in the future", []string{HeaderIfModifiedSince, future}, 304, "", false},
		{"If-Range with another ETag", []string{"If-Range", `"x"`}, 200, dRangeBody, false},
		{"If-Range with a date in the past", []string{"If-Range", past}, 200, dRangeBody, false},
	}

	for _, header := range []string{"bytes=", "bytes=-0", "bytes=0-1,+1-2", "bytes=0-1,--0"} {
		for _, c := range conditions {
			t.Run(header+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				rec := dSend(s, http.MethodGet, "/f", append([]string{HeaderRange, header}, c.kv...)...)

				ehttptest.Eq(t, "RFC 9110 §13.2.2", rec.Code, c.code)

				if c.code != 416 && c.code != 412 {
					ehttptest.Eq(t, "RFC 9110 §13.2.2", rec.Body.String(), c.body)
				}

				if c.hasRange {
					ehttptest.Eq(t, "RFC 9110 §15.5.17", rec.Header().Get(HeaderContentRange), "bytes */10")
				}
			})
		}
	}
}
