package security_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

// One test per attack. A KnownBug test skips while the attack still works.

const cSecret = "TOP-SECRET-OUTSIDE-ROOT"

func cHost(ts *httptest.Server) string { return strings.TrimPrefix(ts.URL, "http://") }

// ---------------------------------------------------------------------------
// Path traversal in SendFile
// ---------------------------------------------------------------------------

func cStaticServer(t testing.TB) (ts *httptest.Server, root string) {
	base := t.TempDir()
	root = filepath.Join(base, "public")

	for p, body := range map[string]string{
		filepath.Join(root, "ok.txt"):     "public-file",
		filepath.Join(base, "secret.txt"): cSecret,
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := ehttp.NewServer()
	// The usual user code: root + wildcard parameter.
	srv.GET("/static/...", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.SendFile(root + "/" + req.Param("..."))
	})

	return fixtures.Start(t, srv), root
}

func TestCSecurityPathTraversal(t *testing.T) {
	ts, _ := cStaticServer(t)

	cases := []struct {
		name     string
		rawPath  string
		knownBug string // set while the secret leaks (none open)
		note     string
	}{
		{"dotdot", "/static/../secret.txt", "", "net/http does not clean the path for custom handlers"},
		{"nested-dotdot", "/static/a/../../secret.txt", "", "dot segments in the middle"},
		{"encoded-dots", "/static/%2e%2e/secret.txt", "", "stays encoded today; becomes ../ once params are decoded, so the guard must live inside SendFile"},
		{"encoded-slash", "/static/..%2fsecret.txt", "", "same as above"},
		{"fully-encoded", "/static/%2e%2e%2fsecret.txt", "", "same as above"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := func(t testing.TB) {
				t.Helper()

				resp := fixtures.Raw(t, cHost(ts), "GET "+tc.rawPath+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
				ehttptest.True(t, "the path stays inside the root ("+tc.note+")", !strings.Contains(resp, cSecret),
					"file outside the served root was returned for %s", tc.rawPath)
			}

			if tc.knownBug != "" {
				ehttptest.KnownBug(t, tc.knownBug, check)
				return
			}

			check(t)
		})
	}

	t.Run("control: file inside root is served", func(t *testing.T) {
		resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/static/ok.txt", nil)
		ehttptest.Eq(t, "control: a file inside the root is served", resp.StatusCode, 200)
		ehttptest.Eq(t, "control: a file inside the root is served", body, "public-file")
	})
}

// A symlink inside the root pointing outside is NOT covered by the
// ".." check. The reference behavior (http.Dir) follows symlinks too.
func TestCSecuritySymlinkMatchesHttpDir(t *testing.T) {
	ts, root := cStaticServer(t)

	if err := os.Symlink(filepath.Join(filepath.Dir(root), "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ref := httptest.NewServer(http.StripPrefix("/static/", http.FileServer(http.Dir(root))))
	t.Cleanup(ref.Close)

	got, gotBody := fixtures.Fetch(t, http.MethodGet, ts.URL+"/static/link.txt", nil)
	want, wantBody := fixtures.Fetch(t, http.MethodGet, ref.URL+"/static/link.txt", nil)

	ehttptest.Eq(t, "net/http http.Dir reference (follows symlinks)", got.StatusCode, want.StatusCode)
	ehttptest.Eq(t, "net/http http.Dir reference (follows symlinks)", gotBody, wantBody)
}

// ---------------------------------------------------------------------------
// X-Forwarded-* from a peer nobody declared trusted
// ---------------------------------------------------------------------------

func cForwardServer(t testing.TB, mw ...ehttp.MiddlewareHandlerFunc) *httptest.Server {
	srv := ehttp.NewServer()
	srv.Use(mw...)
	srv.GET("/who", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String(fmt.Sprintf("proto=%s|host=%s|hostname=%s", req.Protocol(), req.Host(), req.Hostname()))
	})

	return fixtures.Start(t, srv)
}

func cWho(t testing.TB, ts *httptest.Server, hdr http.Header) (proto, host, hostname string) {
	t.Helper()

	_, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/who", hdr)
	parts := strings.Split(body, "|")

	if len(parts) != 3 {
		t.Fatalf("unexpected body %q", body)
	}

	return strings.TrimPrefix(parts[0], "proto="), strings.TrimPrefix(parts[1], "host="), strings.TrimPrefix(parts[2], "hostname=")
}

func TestCSecuritySpoofedForwardedHeaders(t *testing.T) {
	ts := cForwardServer(t)
	spoof := http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"evil.example"}, "X-Forwarded-For": {"6.6.6.6"}}

	t.Run("Protocol ignores X-Forwarded-Proto", func(t *testing.T) {
		proto, _, _ := cWho(t, ts, spoof)
		ehttptest.Eq(t, "untrusted peer cannot claim https", proto, "http")
	})

	t.Run("Host ignores X-Forwarded-Host", func(t *testing.T) {
		_, host, _ := cWho(t, ts, spoof)
		ehttptest.Eq(t, "password-reset links must not use a forged host", host, cHost(ts))
	})

	t.Run("Hostname is the request host without port", func(t *testing.T) {
		_, _, hostname := cWho(t, ts, nil)
		ehttptest.Eq(t, "Hostname() comes from the request on a server", hostname, "127.0.0.1")
	})

	t.Run("Protocol without proxy headers is http", func(t *testing.T) {
		proto, _, _ := cWho(t, ts, nil)
		ehttptest.Eq(t, "Protocol() comes from the request on a server", proto, "http")
	})

	t.Run("Host without proxy headers is the Host header", func(t *testing.T) {
		_, host, _ := cWho(t, ts, nil)
		ehttptest.Eq(t, "RFC 9110 §7.2 Host", host, cHost(ts))
	})
}

// RFC 6797 §7.2: a server must not send HSTS over a non-secure transport.
// (Severity is low: §8.1 makes user agents ignore it there.)
func TestCSecuritySpoofedProtoTriggersHSTS(t *testing.T) {
	ts := cForwardServer(t, middleware.Secure(middleware.SecureOptions{HSTSMaxAge: 3600}))

	resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/who", http.Header{"X-Forwarded-Proto": {"https"}})

	ehttptest.Eq(t, "RFC 6797 §7.2 (HSTS only over a secure transport)", resp.Header.Get("Strict-Transport-Security"), "")

	plain, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/who", nil)
	ehttptest.Eq(t, "RFC 6797 §7.2: plain HTTP gets no HSTS", plain.Header.Get("Strict-Transport-Security"), "")
}

// ---------------------------------------------------------------------------
// CORS attacks
// ---------------------------------------------------------------------------

func TestCSecurityCorsReflection(t *testing.T) {
	cases := []struct {
		name   string
		origin string
	}{
		{"unlisted origin", fixtures.OriginEvil},
		{"null origin (sandboxed iframe)", "null"},
		{"suffix lookalike", fixtures.OriginA + ".evil.com"},
		{"prefix lookalike", "https://evil-app.example"},
		{"different port", fixtures.OriginA + ":8443"},
		{"different scheme", "http://app.example"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}, Credentials: true})
			ts := fixtures.Start(t, srv)

			resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/api", http.Header{"Origin": {tc.origin}})
			acao := resp.Header.Get("Access-Control-Allow-Origin")
			acac := resp.Header.Get("Access-Control-Allow-Credentials")

			ehttptest.True(t, "Fetch §3.2.5: authenticated cross-origin read must not be granted",
				acao != tc.origin || acac != "true", "ACAO=%q ACAC=%q for Origin %q", acao, acac, tc.origin)
		})
	}
}

func TestCSecurityWildcardWithCredentials(t *testing.T) {
	t.Run("evil origin gets credentials", func(t *testing.T) {
		ts := fixtures.Start(t, fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}, Credentials: true}))

		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/api", http.Header{"Origin": {fixtures.OriginEvil}})

		ehttptest.True(t, "Fetch §3.2.5: wildcard + credentials must never reflect the origin",
			resp.Header.Get("Access-Control-Allow-Origin") != fixtures.OriginEvil || resp.Header.Get("Access-Control-Allow-Credentials") != "true",
			"ACAO=%q ACAC=%q", resp.Header.Get("Access-Control-Allow-Origin"), resp.Header.Get("Access-Control-Allow-Credentials"))
	})

	t.Run("boot fails fast", func(t *testing.T) {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			ehttp.Cors(ehttp.CorsOptions{Credentials: true})

			return false
		}()

		ehttptest.True(t, "fail fast at boot: Credentials true with a wildcard origin", panicked, "Credentials:true with wildcard origin did not panic")
	})
}

func TestCSecurityPreflightRejectsLookalikes(t *testing.T) {
	for _, origin := range []string{fixtures.OriginEvil, "null", fixtures.OriginA + ".evil.com", "https://evil-app.example"} {
		t.Run(origin, func(t *testing.T) {
			ts := fixtures.Start(t, fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}}))

			resp, _ := fixtures.Fetch(t, http.MethodOptions, ts.URL+"/api", http.Header{
				"Origin":                        {origin},
				"Access-Control-Request-Method": {"GET"},
				"Sec-Fetch-Site":                {"cross-site"}, // what a browser sends
			})

			ehttptest.Eq(t, "Fetch §4.8: preflight from a non-allowlisted origin fails", resp.StatusCode, 403)
		})
	}
}

// Sec-Fetch-Site is a forbidden header name for browsers (Fetch §2.2.1), so only
// non-browser clients can forge it, and they do not need CORS. Reproduced, but
// not exploitable cross-site: severity downgraded.
func TestCSecurityForgedSecFetchSite(t *testing.T) {
	ts := fixtures.Start(t, fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}}))

	resp, _ := fixtures.Fetch(t, http.MethodOptions, ts.URL+"/api", http.Header{
		"Origin":                        {fixtures.OriginEvil},
		"Access-Control-Request-Method": {"GET"},
		"Sec-Fetch-Site":                {"same-origin"},
	})

	ehttptest.Eq(t, "allowlist must not be bypassed by a self-declared Sec-Fetch-Site", resp.StatusCode, 403)
}

// CORS is off until the developer adds the Cors middleware: a server without
// it sends no CORS header, and another server's policy does not reach it.
func TestCSecurityCorsOffByDefault(t *testing.T) {
	_ = fixtures.CorsServer(t, &ehttp.CorsOptions{Origin: []string{fixtures.OriginA}})
	other := ehttp.NewServer()
	other.GET("/api", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("x") })
	ts := fixtures.Start(t, other)

	resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/api", http.Header{"Origin": {fixtures.OriginA}})
	ehttptest.Eq(t, "Fetch §3.2: no Cors middleware, no CORS header", resp.Header.Get("Access-Control-Allow-Origin"), "")

	resp, _ = fixtures.Fetch(t, http.MethodOptions, ts.URL+"/api", http.Header{
		"Origin":                        {fixtures.OriginB},
		"Access-Control-Request-Method": {"GET"},
	})
	ehttptest.Eq(t, "RFC 9110 §9.3.7: OPTIONS without Cors is a plain answer", resp.StatusCode, 200)
	ehttptest.Eq(t, "Fetch §3.2: no Cors middleware, no CORS header", resp.Header.Get("Access-Control-Allow-Origin"), "")
	ehttptest.Eq(t, "Fetch §3.2: no Cors middleware, no CORS header", resp.Header.Get("Access-Control-Allow-Methods"), "")
}

// ---------------------------------------------------------------------------
// Group middleware
// ---------------------------------------------------------------------------

func cPanics(f func()) (p bool) {
	defer func() { p = recover() != nil }()

	f()

	return false
}

func cAuth() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			if req.Header("Authorization") == "" {
				res.SendStatus(401)
				return
			}

			next.ServeHTTP(res, req)
		}
	}
}

func cMark(name string) ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.Header().Set(name, "1")
			next.ServeHTTP(res, req)
		}
	}
}

func cSecretHandler(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("secret") }

func TestCSecurityGroupAuthBypass(t *testing.T) {
	t.Run("Route(prefix, auth) protects the group", func(t *testing.T) {
		srv := ehttp.NewServer()
		srv.Route("/admin", cAuth()).GET("/secret", cSecretHandler)
		ts := fixtures.Start(t, srv)

		resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/admin/secret", nil)
		ehttptest.Eq(t, "group middleware must run", resp.StatusCode, 401)
		ehttptest.True(t, "group middleware rejects the request before the handler", !strings.Contains(body, "secret"), "secret body leaked: %q", body)
	})

	t.Run("nested Route keeps parent and own middleware", func(t *testing.T) {
		srv := ehttp.NewServer()
		admin := srv.Route("/admin", cAuth())
		admin.Route("/users", cMark("X-Nested")).GET("/1", cSecretHandler)
		ts := fixtures.Start(t, srv)

		t.Run("inherits the parent auth", func(t *testing.T) {
			resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/admin/users/1", nil)
			ehttptest.Eq(t, "nested group inherits auth", resp.StatusCode, 401)
		})

		t.Run("runs its own middleware", func(t *testing.T) {
			resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/admin/users/1", http.Header{"Authorization": {"x"}})
			ehttptest.Eq(t, "nested group's own middleware runs", resp.Header.Get("X-Nested"), "1")
		})
	})

	t.Run("controls: server Use and per-route middleware do protect", func(t *testing.T) {
		srv := ehttp.NewServer()
		srv.Use(cAuth())
		srv.GET("/a", cSecretHandler)
		ts := fixtures.Start(t, srv)

		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/a", nil)
		ehttptest.Eq(t, "control: server.Use", resp.StatusCode, 401)

		srv2 := ehttp.NewServer()
		srv2.GET("/b", cSecretHandler, cAuth())
		ts2 := fixtures.Start(t, srv2)

		resp, _ = fixtures.Fetch(t, http.MethodGet, ts2.URL+"/b", nil)
		ehttptest.Eq(t, "control: per-route middleware", resp.StatusCode, 401)

		resp, body := fixtures.Fetch(t, http.MethodGet, ts2.URL+"/b", http.Header{"Authorization": {"x"}})
		ehttptest.Eq(t, "control: authorized request passes", resp.StatusCode, 200)
		ehttptest.Eq(t, "control: authorized request passes", body, "secret")
	})

	t.Run("Use after a route panics (go-chi order)", func(t *testing.T) {
		srv := ehttp.NewServer()
		srv.GET("/early", cSecretHandler)

		ehttptest.True(t, "chi mux.go: all middlewares must be defined before routes", cPanics(func() { srv.Use(cAuth()) }), "Use after a route did not panic")
	})
}

func TestCSecurityGroupUseLeaksToWholeServer(t *testing.T) {
	srv := ehttp.NewServer()
	srv.GET("/public", cSecretHandler)

	api := srv.Route("/api")
	api.Use(cMark("X-Group"))
	api.GET("/x", cSecretHandler)

	ts := fixtures.Start(t, srv)

	t.Run("control: group route gets the middleware", func(t *testing.T) {
		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/api/x", nil)
		ehttptest.Eq(t, "control", resp.Header.Get("X-Group"), "1")
	})

	t.Run("route outside the group", func(t *testing.T) {
		// public was registered before api.Use, so only 404/405/OPTIONS expose the leak.
		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/missing", nil)
		ehttptest.Eq(t, "group middleware must not run on unrelated 404s", resp.Header.Get("X-Group"), "")
	})

	t.Run("route registered after Use outside the group", func(t *testing.T) {
		srv := ehttp.NewServer()
		api := srv.Route("/api")
		api.Use(cMark("X-Group"))
		srv.GET("/later", cSecretHandler)
		ts := fixtures.Start(t, srv)

		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL+"/later", nil)
		ehttptest.Eq(t, "sub.Use is scoped to the group", resp.Header.Get("X-Group"), "")
	})
}

// ---------------------------------------------------------------------------
// Recover
// ---------------------------------------------------------------------------

func cRecoverServer(t testing.TB) *httptest.Server {
	srv := ehttp.NewServer()
	srv.Use(middleware.Recover())
	srv.GET("/leak", func(res *ehttp.ResponseWriter, req *ehttp.Request) { panic("db password=hunter2") })
	srv.GET("/abort", func(res *ehttp.ResponseWriter, req *ehttp.Request) { panic(http.ErrAbortHandler) })
	srv.GET("/partial", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		res.String("partial")
		panic("late failure")
	})
	srv.GET("/stream", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		_, _ = res.Write([]byte("chunk-1;")) // no Content-Length: chunked
		res.Flush()
		panic("late failure")
	})

	return fixtures.Start(t, srv)
}

func TestCSecurityPanicDoesNotLeakToClient(t *testing.T) {
	ts := cRecoverServer(t)

	resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/leak", nil)
	ehttptest.Eq(t, "panic becomes 500", resp.StatusCode, 500)

	ehttptest.True(t, "panic value is internal data", !strings.Contains(body, "hunter2"), "body leaks panic value: %s", body)
}

func TestCSecurityAbortHandlerIsRethrown(t *testing.T) {
	ts := cRecoverServer(t)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/abort", http.NoBody)
	resp, err := fixtures.Client().Do(req)
	if err == nil {
		resp.Body.Close()
	}

	ehttptest.True(t, "net/http ErrAbortHandler convention: the response is aborted", err != nil,
		"client got status %d instead of an aborted connection", respStatus(resp))
}

func respStatus(r *http.Response) int {
	if r == nil {
		return 0
	}

	return r.StatusCode
}

func TestCSecurityPanicAfterHeadersSent(t *testing.T) {
	ts := cRecoverServer(t)

	t.Run("sized body: net/http Content-Length protects the client", func(t *testing.T) {
		resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/partial", nil)
		ehttptest.Eq(t, "premise: the first body is sent", resp.StatusCode, 200)
		ehttptest.Eq(t, "premise: only what was sent before the panic", body, "partial")
	})

	// The correct fix aborts the stream (re-panic http.ErrAbortHandler): the
	// client sees an incomplete message (RFC 9112 §8), e.g. io.ErrUnexpectedEOF.
	// Without the abort, the 500 JSON is glued to the body already in flight.
	t.Run("chunked body is aborted, not completed with the error JSON", func(t *testing.T) {
		// The handler flushes first, so the status line and headers must arrive.
		// A dial error or a timeout here is a real failure, not the fix.
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/stream", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}

		resp, err := fixtures.Client().Do(req)
		if err != nil {
			t.Fatalf("headers did not arrive: %v", err)
		}
		defer resp.Body.Close()

		b, readErr := io.ReadAll(resp.Body)

		incomplete := errors.Is(readErr, io.ErrUnexpectedEOF)
		cleanWithoutJSON := readErr == nil && !strings.Contains(string(b), `"code"`)

		ehttptest.True(t, "RFC 9112 §8: incomplete message (io.ErrUnexpectedEOF), or a complete one without the error JSON",
			incomplete || cleanWithoutJSON, "body = %q, read error = %v", b, readErr)
	})
}

// ---------------------------------------------------------------------------
// X-Request-Id
// ---------------------------------------------------------------------------

func TestCSecurityRequestId(t *testing.T) {
	srv := ehttp.NewServer()
	srv.Use(middleware.RequestId())
	srv.GET("/", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("ok") })
	ts := fixtures.Start(t, srv)

	// An incoming id is kept only with 1 to 128
	// characters of [A-Za-z0-9._-]; otherwise a new id that follows the same rule.
	validID := regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

	for name, id := range map[string]string{
		"64 KiB id":  strings.Repeat("a", 64*1024),
		"129 chars":  strings.Repeat("a", 129),
		"with space": "a b",
		"with slash": "a/b",
	} {
		t.Run(name+" is replaced by a valid id", func(t *testing.T) {
			resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL, http.Header{"X-Request-Id": {id}})
			got := resp.Header.Get("X-Request-Id")

			ehttptest.True(t, "the untrusted id is not echoed", got != id, "echoed %d bytes", len(got))
			ehttptest.True(t, "the new id follows the same rule", validID.MatchString(got), "id = %q", got)
		})
	}

	t.Run("a valid 128-char id is kept", func(t *testing.T) {
		id := strings.Repeat("a", 128)
		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL, http.Header{"X-Request-Id": {id}})
		ehttptest.Eq(t, "1 to 128 characters are kept", resp.Header.Get("X-Request-Id"), id)
	})

	t.Run("missing id gets a generated one", func(t *testing.T) {
		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL, nil)
		ehttptest.True(t, "RequestId contract", len(resp.Header.Get("X-Request-Id")) == 36, "id = %q", resp.Header.Get("X-Request-Id"))
	})

	t.Run("sane id is kept", func(t *testing.T) {
		resp, _ := fixtures.Fetch(t, http.MethodGet, ts.URL, http.Header{"X-Request-Id": {"abc-123"}})
		ehttptest.Eq(t, "RequestId contract", resp.Header.Get("X-Request-Id"), "abc-123")
	})

	// RFC 9112 §5.2: obs-fold and a bare LF must not let a request header
	// smuggle a second header into the response. net/http folds the first
	// into one value and drops the second today; the test pins that.
	for name, raw := range map[string]string{
		"obs-fold":     "GET / HTTP/1.1\r\nHost: x\r\nX-Request-Id: abc\r\n Set-Cookie: pwn=1\r\nConnection: close\r\n\r\n",
		"bare LF":      "GET / HTTP/1.1\r\nHost: x\r\nX-Request-Id: abc\nSet-Cookie: pwn=1\r\nConnection: close\r\n\r\n",
		"bare CR":      "GET / HTTP/1.1\r\nHost: x\r\nX-Request-Id: abc\rSet-Cookie: pwn=1\r\nConnection: close\r\n\r\n",
		"obs-fold tab": "GET / HTTP/1.1\r\nHost: x\r\nX-Request-Id: abc\r\n\tSet-Cookie: pwn=1\r\nConnection: close\r\n\r\n",
	} {
		t.Run("CRLF injection via "+name+" cannot split the response", func(t *testing.T) {
			rawResp := fixtures.Raw(t, cHost(ts), raw)
			head, _, _ := strings.Cut(rawResp, "\r\n\r\n")

			resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(rawResp)), nil)
			if err != nil {
				t.Fatalf("response is not valid HTTP: %v (%q)", err, rawResp)
			}
			defer resp.Body.Close()

			id := resp.Header.Get("X-Request-Id")

			ehttptest.Eq(t, "RFC 9112 §5.2: no injected Set-Cookie", resp.Header.Get("Set-Cookie"), "")

			for _, line := range strings.Split(head, "\r\n") {
				ehttptest.True(t, "RFC 9112 §5.2: no injected Set-Cookie line", !strings.HasPrefix(strings.ToLower(line), "set-cookie:"), "response head: %q", head)
			}

			ehttptest.True(t, "RFC 9112 §5.2: echoed id has no CR or LF", !strings.ContainsAny(id, "\r\n"), "id = %q", id)
		})
	}

	t.Run("control byte in id never reaches the response", func(t *testing.T) {
		raw := fixtures.Raw(t, cHost(ts), "GET / HTTP/1.1\r\nHost: x\r\nX-Request-Id: a\x01b\r\nConnection: close\r\n\r\n")

		ehttptest.True(t, "premise: net/http rejects control bytes (400) or the id is dropped",
			strings.HasPrefix(raw, "HTTP/1.1 400") || !strings.Contains(raw, "\x01"), "response: %q", raw)
	})
}

// ---------------------------------------------------------------------------
// Route regex
// ---------------------------------------------------------------------------

func TestCSecurityUnanchoredRouteRegex(t *testing.T) {
	srv := ehttp.NewServer()
	srv.GET("/items/:id|[0-9]+", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("id=" + req.Param("id")) })
	ts := fixtures.Start(t, srv)

	resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/items/42", nil)
	ehttptest.Eq(t, "control: a matching segment is served", resp.StatusCode, 200)
	ehttptest.Eq(t, "control: a matching segment is served", body, "id=42")

	for _, bad := range []string{"abc1x", "5;drop", "x9", "9x"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/items/"+bad, nil)
			ehttptest.Eq(t, "regex param must match the whole segment ("+body+")", resp.StatusCode, 404)
		})
	}

	t.Run("invalid regex fails at registration", func(t *testing.T) {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			ehttp.NewServer().GET("/x/:id|[0-9", func(res *ehttp.ResponseWriter, req *ehttp.Request) {})

			return false
		}()

		ehttptest.True(t, "fail fast at boot: an invalid regex panics", panicked, "invalid regexp registered without panic")
	})
}

// ---------------------------------------------------------------------------
// BodyLimit
// ---------------------------------------------------------------------------

// cBodyServer returns the server, the count of handler calls and the count
// of reads that failed with ErrRequestBodyTooLarge. The handler propagates
// that error as 413 itself: BodyLimit does not write 413 for chunked bodies.
func cBodyServer(t testing.TB, limit string) (ts *httptest.Server, called, tooLarge *atomic.Int32) {
	called, tooLarge = new(atomic.Int32), new(atomic.Int32)

	srv := ehttp.NewServer()
	srv.POST("/up", func(res *ehttp.ResponseWriter, req *ehttp.Request) {
		called.Add(1)

		b, err := req.Body()
		if errors.Is(err, ehttp.ErrRequestBodyTooLarge) {
			tooLarge.Add(1)
			res.SendStatus(413)
			return
		}

		res.String(fmt.Sprint(len(b)))
	}, middleware.BodyLimit(limit))

	return fixtures.Start(t, srv), called, tooLarge
}

func cPost(t testing.TB, url string, body io.Reader, length int64) (resp *http.Response, respBody string) {
	t.Helper()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	req.ContentLength = length

	resp, err := fixtures.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return resp, string(b)
}

func TestCSecurityBodyLimit(t *testing.T) {
	ts, called, tooLarge := cBodyServer(t, "1KB")

	t.Run("declared length above limit is 413 without running the handler", func(t *testing.T) {
		resp, _ := cPost(t, ts.URL+"/up", strings.NewReader(strings.Repeat("a", 2048)), 2048)
		ehttptest.Eq(t, "RFC 9110 §15.5.14 413", resp.StatusCode, 413)
		ehttptest.Eq(t, "handler not reached", called.Load(), int32(0))
	})

	// BodyLimit cannot reject a chunked body up front. Its contract is that
	// the handler's read fails with ErrRequestBodyTooLarge; the handler maps
	// that to 413 (RFC 9110 §15.5.14).
	t.Run("chunked body above limit: handler read fails with ErrRequestBodyTooLarge, handler's 413 reaches the client", func(t *testing.T) {
		before := tooLarge.Load()
		resp, _ := cPost(t, ts.URL+"/up", io.NopCloser(strings.NewReader(strings.Repeat("a", 64*1024))), -1)
		ehttptest.Eq(t, "ErrRequestBodyTooLarge contract: read error reached the handler", tooLarge.Load(), before+1)
		ehttptest.Eq(t, "RFC 9110 §15.5.14 (handler propagates the error)", resp.StatusCode, 413)
	})

	t.Run("body at the limit passes", func(t *testing.T) {
		resp, body := cPost(t, ts.URL+"/up", strings.NewReader(strings.Repeat("a", 1024)), 1024)
		ehttptest.Eq(t, "a body at the limit is accepted", resp.StatusCode, 200)
		ehttptest.Eq(t, "a body at the limit is accepted", body, "1024")
	})

	t.Run("limit with a space fails at boot", func(t *testing.T) {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			middleware.BodyLimit("4 KB")

			return false
		}()

		ehttptest.True(t, "invalid limit must fail at boot", panicked, `BodyLimit("4 KB") accepted`)
	})

	t.Run("unparsable limit fails at boot", func(t *testing.T) {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			middleware.BodyLimit("lots")

			return false
		}()

		ehttptest.True(t, "invalid limit must fail at boot", panicked, `BodyLimit("lots") accepted`)
	})
}

// ---------------------------------------------------------------------------
// Slowloris (ReadHeaderTimeout)
// ---------------------------------------------------------------------------

// The default ReadHeaderTimeout is 10s, and the probe is a half-open
// connection that never ends its headers, so the test needs ~12s:
// opt in with EHTTP_SLOW_TESTS=1 (nightly), skipped otherwise to keep the suite fast.
func TestCSecuritySlowloris(t *testing.T) {
	if os.Getenv("EHTTP_SLOW_TESTS") == "" {
		t.Skip("testability: needs >10s (proposed ReadHeaderTimeout default); set EHTTP_SLOW_TESTS=1")
	}

	l := fixtures.Listen(t, func(port string) *ehttp.Server {
		srv := ehttp.NewServer(ehttp.WithPort(port))
		srv.GET("/", func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String("ok") })

		return srv
	})

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", "127.0.0.1:"+l.Port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nX-Slow: "))

	_ = conn.SetReadDeadline(time.Now().Add(13 * time.Second))
	_, readErr := conn.Read(make([]byte, 1))

	var nerr net.Error

	closedByServer := readErr != nil && (!errors.As(readErr, &nerr) || !nerr.Timeout())

	ehttptest.True(t, "the server drops a connection that never finishes its headers",
		closedByServer, "connection still open after 13s")
}
