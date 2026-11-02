package ehttp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"

	"go.uber.org/zap/zaptest/observer"
)

type bSeen struct {
	Method, Path, Query, ContentType, Auth, Custom, Body string
	ContentLength                                        int64
}

// bEchoServer records the last request it got and answers body with status.
func bEchoServer(t *testing.T, status int, body string) (srv *httptest.Server, seen func() bSeen) {
	t.Helper()

	var mu sync.Mutex
	var last bSeen

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		last = bSeen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Header.Get("X-Custom"), string(b), r.ContentLength}
		mu.Unlock()

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return srv, func() bSeen { mu.Lock(); defer mu.Unlock(); return last }
}

func TestClientExecute(t *testing.T) {
	t.Parallel()

	t.Run("GET returns status, headers and body", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "pong")
		c := ehttp.NewClient()

		res, err := c.Execute(ehttp.WithSource("GET", srv.URL+"/ping"))
		if err != nil {
			t.Fatal(err)
		}

		body, _ := res.String()

		ehttptest.Eq(t, "Client contract", res.StatusCode, 200)
		ehttptest.Eq(t, "Client contract", body, "pong")
		ehttptest.Eq(t, "Client contract", seen().Method, "GET")
		ehttptest.Eq(t, "Client contract", seen().Path, "/ping")
		ehttptest.Eq(t, "RFC 9110 §8.6 (no body, no length)", seen().ContentLength, 0)
	})

	t.Run("error statuses are not Go errors", func(t *testing.T) {
		t.Parallel()

		srv, _ := bEchoServer(t, 503, "down")
		res, err := ehttp.NewClient().Execute(ehttp.WithSource("GET", srv.URL))

		ehttptest.True(t, "net/http Client.Do contract", err == nil, "err = %v", err)
		ehttptest.Eq(t, "net/http Client.Do contract", res.StatusCode, 503)
	})

	t.Run("base URL joins path and merges query", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "")
		c := ehttp.NewClient(ehttp.WithBaseUrl(srv.URL + "/api"))

		_, err := c.Execute(ehttp.WithSource("GET", "/users?id=7"))
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "Client contract", seen().Path, "/api/users")
		ehttptest.Eq(t, "Client contract", seen().Query, "id=7")
	})

	t.Run("client Authorization and per-request headers are sent", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "")
		c := ehttp.NewClient(ehttp.WithAuthorization("Bearer abc"))

		_, err := c.Execute(ehttp.WithSource("GET", srv.URL), ehttp.WithHeader("X-Custom", "v1"))
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "Client contract", seen().Auth, "Bearer abc")
		ehttptest.Eq(t, "Client contract", seen().Custom, "v1")
	})

	t.Run("a request header overrides the client Authorization", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "")
		c := ehttp.NewClient(ehttp.WithAuthorization("Bearer abc"))

		_, err := c.Execute(ehttp.WithSource("GET", srv.URL), ehttp.WithHeader("Authorization", "Bearer other"))
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "Client contract (specific beats default)", seen().Auth, "Bearer other")
	})

	t.Run("string payload is sent verbatim", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "")
		_, err := ehttp.NewClient().Execute(ehttp.WithSource("POST", srv.URL), ehttp.WithStringPayload("a=1&b=2"))
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "Client contract", seen().Method, "POST")
		ehttptest.Eq(t, "Client contract", seen().Body, "a=1&b=2")
	})

	t.Run("JSON payload is encoded", func(t *testing.T) {
		t.Parallel()

		srv, seen := bEchoServer(t, 200, "")
		_, err := ehttp.NewClient().Execute(ehttp.WithSource("POST", srv.URL), ehttp.WithJsonPayload(map[string]int{"n": 1}))
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "Client contract", strings.TrimSpace(seen().Body), `{"n":1}`)
	})

	t.Run("cancelled context returns an error", func(t *testing.T) {
		t.Parallel()

		srv, _ := bEchoServer(t, 200, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := ehttp.NewClient().Execute(ehttp.WithSource("GET", srv.URL), ehttp.WithContext(ctx))

		ehttptest.True(t, "context contract", errors.Is(err, context.Canceled), "err = %v, want context.Canceled", err)
	})

	t.Run("invalid method becomes an error", func(t *testing.T) {
		t.Parallel()

		res, err := ehttp.NewClient().Execute(ehttp.WithSource("BAD METHOD", "http://127.0.0.1:1"))

		ehttptest.True(t, "net/http NewRequest contract", err != nil, "no error for an invalid method")
		ehttptest.True(t, "net/http NewRequest contract", res == nil, "response should be nil on a request build error")
	})

	t.Run("WithTimeout is honoured", func(t *testing.T) {
		t.Parallel()

		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(block) })

		_, err := ehttp.NewClient(ehttp.WithTimeout(50 * time.Millisecond)).Execute(ehttp.WithSource("GET", srv.URL))

		var ne net.Error

		ehttptest.True(t, "http.Client.Timeout contract (net.Error.Timeout)", errors.As(err, &ne) && ne.Timeout(), "err = %v, want a timeout error", err)
	})
}

// JSON payloads must say so (RFC 9110 §8.3): a receiver may not parse
// content without a Content-Type.
func TestClientJSONPayloadContentType(t *testing.T) {
	t.Parallel()

	var ct atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ct.Store(r.Header.Get("Content-Type")) }))
	t.Cleanup(srv.Close)

	_, err := ehttp.NewClient().Execute(ehttp.WithSource("POST", srv.URL), ehttp.WithJsonPayload(map[string]int{"n": 1}))
	if err != nil {
		t.Fatal(err)
	}

	ehttptest.Eq(t, "RFC 9110 §8.3", fmt.Sprint(ct.Load()), "application/json")
}

// bDeadlineSeen runs one request and returns the time left on the request
// context seen by the transport (via its Proxy hook). http.Client.Timeout shows up there as a deadline,
// so the default can be read without waiting for it to expire.
func bDeadlineSeen(t *testing.T, mk func(tr *http.Transport) *ehttp.Client) (time.Duration, bool) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)

	var left atomic.Int64
	var has atomic.Bool
	tr := &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) {
		if dl, ok := r.Context().Deadline(); ok {
			left.Store(int64(time.Until(dl)))
			has.Store(true)
		}

		return nil, nil
	}}
	defer tr.CloseIdleConnections()

	res, err := mk(tr).Execute(ehttp.WithSource("GET", srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	res.Body.Close()

	return time.Duration(left.Load()), has.Load()
}

// A client without a timeout hangs forever on a silent server.
func TestClientDefaultTimeout(t *testing.T) {
	t.Parallel()

	t.Run("harness sees an explicit timeout", func(t *testing.T) {
		t.Parallel()

		left, has := bDeadlineSeen(t, func(tr *http.Transport) *ehttp.Client {
			return ehttp.NewClient(ehttp.WithTransport(tr), ehttp.WithTimeout(7*time.Second))
		})

		ehttptest.True(t, "http.Client.Timeout contract", has && left > 0 && left <= 7*time.Second, "has=%v left=%v", has, left)
	})

	t.Run("default client has a bounded timeout", func(t *testing.T) {
		t.Parallel()

		left, has := bDeadlineSeen(t, func(tr *http.Transport) *ehttp.Client {
			return ehttp.NewClient(ehttp.WithTransport(tr))
		})

		ehttptest.True(t, "a client has a default deadline", has, "no deadline: a silent server blocks the caller forever")
		ehttptest.True(t, "the default deadline is bounded", has && left <= 5*time.Minute, "deadline %v is not bounded", left)
	})
}

// On failure the caller gets (nil, err).
func TestClientErrorReturnsNilResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	res, err := ehttp.NewClient().Execute(ehttp.WithSource("GET", addr))

	ehttptest.True(t, "net/http Client.Do contract", err != nil, "no error from a closed server")
	ehttptest.True(t, "net/http convention: a response is nil when an error is returned", res == nil, "response is non-nil (%T) alongside an error", res)
}

// A bad URL is a runtime input error, not a reason to panic the caller.
func TestClientInvalidURLIsAnError(t *testing.T) {
	t.Parallel()

	_, err := ehttp.NewClient().Execute(ehttp.WithSource("GET", "http://[::1"))

	ehttptest.True(t, "a bad URL is an error, not a panic", err != nil, "no error for an unparsable URL")
}

type bTrackBody struct {
	r      io.Reader
	closed bool
	eof    bool
}

func (b *bTrackBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.eof = true
	}

	return n, err
}

func (b *bTrackBody) Close() error { b.closed = true; return nil }

// Response helpers consume the body, so they must close it, otherwise
// the connection never goes back to the pool.
func TestResponseHelpersCloseBody(t *testing.T) {
	t.Parallel()

	reads := map[string]func(*ehttp.Response) error{
		"String": func(r *ehttp.Response) error { _, err := r.String(); return err },
		"Bytes":  func(r *ehttp.Response) error { _, err := r.Bytes(); return err },
		"JSON":   func(r *ehttp.Response) error { var v map[string]int; return r.JSON(&v) },
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := &bTrackBody{r: strings.NewReader(`{"a":1}`)}
			res := &ehttp.Response{Response: &http.Response{Body: body}}

			if err := read(res); err != nil {
				t.Fatal(err)
			}

			ehttptest.True(t, "net/http Response.Body doc: the reader closes the body", body.closed, "%s left the body open", name)
		})
	}

	t.Run("JSON reads to EOF so the connection can be reused", func(t *testing.T) {
		t.Parallel()

		body := &bTrackBody{r: strings.NewReader(`{"a":1}` + strings.Repeat(" ", 4096))}
		res := &ehttp.Response{Response: &http.Response{Body: body}}

		var v map[string]int
		if err := res.JSON(&v); err != nil {
			t.Fatal(err)
		}

		ehttptest.True(t, "net/http Response.Body doc: read to EOF to reuse the connection", body.eof, "JSON stopped before EOF")
	})
}

// Sequential calls through String/JSON should share one
// connection.
func TestClientReusesConnection(t *testing.T) {
	t.Parallel()

	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"a":1}`+strings.Repeat(" ", 200_000))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := ehttp.NewClient(ehttp.WithTransport(&http.Transport{}))

	for range 3 {
		res, err := c.Execute(ehttp.WithSource("GET", srv.URL))
		if err != nil {
			t.Fatal(err)
		}

		var v map[string]int
		if err := res.JSON(&v); err != nil {
			t.Fatal(err)
		}
	}

	ehttptest.Eq(t, "net/http Response.Body doc: a drained body lets the connection be reused", conns.Load(), 1)
}

// Tokens and credentials often travel in the URL; they must not reach the log.
func TestClientDoesNotLogQuery(t *testing.T) {
	const secrets = "pw secret frag"

	assertClean := func(t *testing.T, logs *observer.ObservedLogs) {
		t.Helper()

		for _, e := range logs.All() {
			text := e.Message + fmt.Sprint(e.ContextMap())
			for _, s := range strings.Fields(secrets) {
				ehttptest.True(t, "the URL secrets stay out of the log", !strings.Contains(text, s), "log %q leaks %q: %v", e.Message, s, e.ContextMap())
			}
		}

		ehttptest.True(t, "harness: something was logged", len(logs.All()) > 0, "no log lines captured")
	}

	logQueryTest := func(t *testing.T, target func(srvURL string) string, closeServer bool) {
		logs := fixtures.CaptureLogs(t)
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := target(srv.URL)

		if closeServer {
			srv.Close()
		} else {
			t.Cleanup(srv.Close)
		}

		res, _ := ehttp.NewClient().Execute(ehttp.WithSource("GET", url))
		if res != nil && res.Response != nil {
			res.Body.Close()
		}

		assertClean(t, logs)
	}

	withSecrets := func(u string) string {
		return strings.Replace(u, "http://", "http://u:pw@", 1) + "/x?token=secret-123#frag"
	}

	for _, closed := range []bool{false, true} {
		name := map[bool]string{false: "success path", true: "failure path"}[closed]

		t.Run(name, func(t *testing.T) {
			logQueryTest(t, func(u string) string { return u + "/x?token=secret-123" }, closed)
		})
		t.Run(name+" with userinfo and fragment", func(t *testing.T) {
			logQueryTest(t, withSecrets, closed)
		})
	}

	t.Run("bad source URL", func(t *testing.T) {
		logs := fixtures.CaptureLogs(t)

		res, err := ehttp.NewClient().Execute(ehttp.WithSource("GET", "http://u:pw@[::1/x?token=secret-123#frag"))

		ehttptest.True(t, "a bad URL is an error", res == nil && err != nil, "res=%v err=%v", res, err)
		ehttptest.True(t, "the returned error hides the URL", !strings.Contains(err.Error(), "pw"), "err = %v", err)
		assertClean(t, logs)
	})

	t.Run("bad base URL panics without the URL in the log", func(t *testing.T) {
		logs := fixtures.CaptureLogs(t)

		func() {
			defer func() { _ = recover() }()

			ehttp.NewClient(ehttp.WithBaseUrl("http://u:pw@[::1/x?token=secret-123#frag"))
		}()

		assertClean(t, logs)
	})
}

// The base URL is a prefix. A request path must stay under it, keep its
// percent-encoding, and keep repeated query keys.
func TestClientBaseURLJoin(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var uris []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.RequestURI)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	// do sends one request and returns the request-target that the server saw
	// (empty when the client refused to send it).
	do := func(target string) (uri string, err error) {
		mu.Lock()
		uris = nil
		mu.Unlock()

		c := ehttp.NewClient(ehttp.WithBaseUrl(srv.URL + "/api/v1"))
		res, err := c.Execute(ehttp.WithSource("GET", target))

		if res != nil && res.Response != nil {
			res.Body.Close()
		}

		mu.Lock()
		defer mu.Unlock()

		if len(uris) > 0 {
			uri = uris[0]
		}

		return uri, err
	}

	for _, target := range []string{"/../admin", "/x/%2e%2e/%2e%2e/admin"} {
		t.Run("stays under the prefix "+target, func(t *testing.T) {
			uri, err := do(target)

			// (a) the client refuses before it sends: no request seen and an
			// error that is not a network failure. A dial or transport error
			// is not a refusal.
			if uri == "" {
				var opErr *net.OpError

				ehttptest.True(t, "RFC 3986 §5.2.4 (refuse before sending)", err != nil && !errors.As(err, &opErr),
					"no request reached the server and err = %v", err)

				return
			}

			// (b) the server saw a path that stays under the prefix.

			p, _, _ := strings.Cut(uri, "?")
			// a server resolves dot segments before routing (RFC 3986 §5.2.4).
			resolved := path.Clean(strings.ReplaceAll(strings.ToLower(p), "%2e", "."))

			ehttptest.True(t, "RFC 3986 §5.2.4 (a base URL is a prefix)", strings.HasPrefix(resolved, "/api/v1/"),
				"server saw %q, which resolves to %q", uri, resolved)
		})
	}

	t.Run("keeps an encoded slash", func(t *testing.T) {
		uri, err := do("/files/a%2Fb")
		if err != nil {
			t.Fatal(err)
		}

		ehttptest.Eq(t, "RFC 3986 §2.2 (%2F is data, not a separator)", uri, "/api/v1/files/a%2Fb")
	})

	t.Run("keeps repeated query keys", func(t *testing.T) {
		uri, err := do("/q?a=1&a=2")
		if err != nil {
			t.Fatal(err)
		}

		_, query, _ := strings.Cut(uri, "?")
		vals, _ := url.ParseQuery(query)

		ehttptest.Eq(t, "RFC 3986 §3.4 (the query is kept whole)", strings.Join(vals["a"], ","), "1,2")
	})
}

func TestResponseGetCacheControlMaxAge(t *testing.T) {
	t.Parallel()

	mk := func(lines ...string) *ehttp.Response {
		h := http.Header{}
		for _, l := range lines {
			h.Add("Cache-Control", l)
		}

		return &ehttp.Response{Response: &http.Response{Header: h}}
	}

	tests := []struct {
		name  string
		lines []string
		want  time.Duration
	}{
		{"absent", nil, 0},
		{"plain", []string{"max-age=60"}, time.Minute},
		{"among other directives", []string{"public, max-age=120, stale-if-error=5"}, 2 * time.Minute},
		{"zero", []string{"max-age=0"}, 0},
		{"not a number", []string{"max-age=abc"}, 0},
		{"s-maxage is not max-age", []string{"s-maxage=30"}, 0},
		{"no-store only", []string{"no-store"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ehttptest.Eq(t, "RFC 9111 §5.2.2.1", mk(tt.lines...).GetCacheControlMaxAge(), tt.want)
		})
	}

	more := []struct {
		name, basis string
		lines       []string
		want        time.Duration
	}{
		{"directive names are case-insensitive", "RFC 9111 §5.2", []string{"MAX-AGE=60"}, time.Minute},
		{"quoted-string form is accepted", "RFC 9111 §5.2", []string{`max-age="60"`}, time.Minute},
		{"negative value is invalid, treated as stale", "RFC 9111 §1.2.2", []string{"max-age=-5"}, 0},
		{"multiple header lines are one list", "RFC 9110 §5.3", []string{"public", "max-age=60"}, time.Minute},
	}

	for _, tt := range more {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ehttptest.Eq(t, tt.basis, mk(tt.lines...).GetCacheControlMaxAge(), tt.want)
		})
	}
}

func TestResponseBodyHelpers(t *testing.T) {
	t.Parallel()

	mk := func(body string) *ehttp.Response {
		return &ehttp.Response{Response: &http.Response{Body: io.NopCloser(strings.NewReader(body))}}
	}

	t.Run("String", func(t *testing.T) {
		t.Parallel()

		got, err := mk("héllo").String()

		ehttptest.True(t, "Response contract", err == nil, "err = %v", err)
		ehttptest.Eq(t, "Response contract", got, "héllo")
	})

	t.Run("Bytes", func(t *testing.T) {
		t.Parallel()

		got, err := mk("abc").Bytes()

		ehttptest.True(t, "Response contract", err == nil, "err = %v", err)
		ehttptest.Eq(t, "Response contract", string(got), "abc")
	})

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()

		s, err := mk("").String()

		ehttptest.True(t, "Response contract", err == nil, "err = %v", err)
		ehttptest.Eq(t, "Response contract", s, "")
	})

	t.Run("JSON into a pointer", func(t *testing.T) {
		t.Parallel()

		var v struct{ A int }
		err := mk(`{"A":3}`).JSON(&v)

		ehttptest.True(t, "Response contract", err == nil, "err = %v", err)
		ehttptest.Eq(t, "Response contract", v.A, 3)
	})

	t.Run("JSON with malformed input returns an error", func(t *testing.T) {
		t.Parallel()

		var v map[string]any
		err := mk(`{"A":`).JSON(&v)

		ehttptest.True(t, "encoding/json Decoder contract", err != nil, "no error for truncated JSON")
	})

	t.Run("read error is returned", func(t *testing.T) {
		t.Parallel()

		res := &ehttp.Response{Response: &http.Response{Body: io.NopCloser(bFailingReader{})}}

		_, err := res.String()
		_, err2 := res.Bytes()

		ehttptest.True(t, "io.ReadAll contract", err != nil && err2 != nil, "errors: %v, %v", err, err2)
	})

	t.Run("JSON into a non-pointer reports an error", func(t *testing.T) {
		t.Parallel()

		var v struct{ A int }
		err := mk(`{"A":3}`).JSON(v)

		ehttptest.True(t, "encoding/json Unmarshal contract (InvalidUnmarshalError)", err != nil, "decoded into a copy and returned nil")
	})
}

type bFailingReader struct{}

func (bFailingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
