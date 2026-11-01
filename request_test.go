package ehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aReq builds a Request without going through the router.
func aReq(method, target string, body io.Reader, kv ...string) *Request {
	r := httptest.NewRequestWithContext(context.Background(), method, target, body)

	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}

	return &Request{Request: r}
}

func TestRequestAccessors(t *testing.T) {
	t.Parallel()

	req := aReq("POST", "/a%2Fb/c?x=1&x=2&y=%20z&flag", nil,
		"X-Custom", "v", "Origin", "https://a.example", "User-Agent", "ua/1.0")
	req.Request.SetPathValue("id", "5")
	req.Request.Pattern = "/a/:id"

	tests := []struct {
		name, basis, got, want string
	}{
		{"Method", "net/http Request.Method", req.Method(), "POST"},
		{"Path keeps the escaped form and drops the query", "RFC 3986 §3.3", req.Path(), "/a%2Fb/c"},
		{"RoutePath", "net/http Request.Pattern", req.RoutePath(), "/a/:id"},
		{"Param", "net/http Request.PathValue", req.Param("id"), "5"},
		{"Param unknown", "net/http Request.PathValue", req.Param("nope"), ""},
		{"Query first value", "Express req.query / url.Values.Get", req.Query("x"), "1"},
		{"Query decoded", "RFC 3986 §2.1", req.Query("y"), " z"},
		{"Query flag without value", "url.Values.Get", req.Query("flag"), ""},
		{"Query missing", "url.Values.Get", req.Query("nope"), ""},
		{"Header", "Express req.get", req.Header("X-Custom"), "v"},
		{"Header is case-insensitive", "RFC 9110 §5.1", req.Header("x-custom"), "v"},
		{"Header missing", "Express req.get", req.Header("X-Nope"), ""},
		{"Origin", "Fetch §3.2.1 (Origin header)", req.Origin(), "https://a.example"},
		{"UserAgent", "RFC 9110 §10.1.5", req.UserAgent(), "ua/1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ehttptest.Eq(t, tt.basis, tt.got, tt.want)
		})
	}

	t.Run("RoutePath is empty before routing", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "net/http Request.Pattern", aReq("GET", "/", nil).RoutePath(), "")
	})
}

func TestRequestAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		accept string
		value  string
		want   bool
		bug    string
		basis  string
	}{
		{"no Accept header accepts anything", "", "application/json", true, "", "RFC 9110 §12.5.1"},
		{"wildcard", "*/*", "text/html", true, "", "RFC 9110 §12.5.1"},
		{"exact", "application/json", "application/json", true, "", "RFC 9110 §12.5.1"},
		{"different type", "application/json", "text/html", false, "", "RFC 9110 §12.5.1"},
		{"value with parameters", "text/html", "text/html; charset=utf-8", true, "", "RFC 9110 §8.3.1"},
		{"media type is case-insensitive", "TEXT/HTML", "text/html", true, "", "RFC 9110 §8.3.1"},
		{"type wildcard", "application/*", "application/json", true, "", "RFC 9110 §12.5.1"},
		{"type wildcard, other type", "application/*", "text/html", false, "", "RFC 9110 §12.5.1"},
		{"subtype wildcard", "text/*", "text/html", true, "", "RFC 9110 §12.5.1"},
		{"text/* must not match application/text", "text/*", "application/text", false, "C8", "RFC 9110 §12.5.1"},
		{"structured suffix is literal", "application/vnd.api+json", "application/vnd.api+json", true, "C8", "RFC 6838 §4.2.8"},
		{"list, first member", "application/json, text/plain", "application/json", true, "C8", "RFC 9110 §12.5.1"},
		{"list, second member", "application/json, text/plain", "text/plain", true, "C8", "RFC 9110 §12.5.1"},
		{"list, absent member", "application/json, text/plain", "text/html", false, "", "RFC 9110 §12.5.1"},
		{"list with weights", "text/html;q=0.9, application/json;q=0.8", "application/json", true, "C8", "RFC 9110 §12.5.1"},
		{"q=0 means not acceptable", "text/html;q=0", "text/html", false, "C8", "RFC 9110 §12.4.2"},
		{"prefix of the value is not a match", "text/html", "text/htmlx", false, "C8", "RFC 9110 §12.5.1"},
		{"dot is not a regex wildcard", "text/h.ml", "text/html", false, "C8", "RFC 9110 §12.5.1"},
		{"alternation is not a regex operator", "text/x|y", "application/y", false, "C8", "RFC 9110 §12.5.1"},
		{"malformed Accept", "garbage", "text/html", false, "", "RFC 9110 §12.5.1 (unusable header)"},
		{"malformed value", "text/html", "garbage", false, "", "RFC 9110 §8.3.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			check := func(t testing.TB) {
				req := aReq("GET", "/", nil)
				if tt.accept != "" {
					req.Request.Header.Set(HeaderAccept, tt.accept)
				}

				ehttptest.Eq(t, tt.basis, req.Accepts(tt.value), tt.want)
			}

			if tt.bug != "" {
				ehttptest.KnownBug(t, tt.bug, check)
				return
			}

			check(t)
		})
	}
}

func TestRequestBody(t *testing.T) {
	t.Parallel()

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()

		b, err := aReq("POST", "/", nil).Body()
		ehttptest.Eq(t, "io.ReadAll", err, nil)
		ehttptest.Eq(t, "io.ReadAll", len(b), 0)

		s, err := aReq("POST", "/", nil).BodyString()
		ehttptest.Eq(t, "io.ReadAll", err, nil)
		ehttptest.Eq(t, "io.ReadAll", s, "")
	})

	t.Run("content", func(t *testing.T) {
		t.Parallel()

		b, err := aReq("POST", "/", strings.NewReader("abc")).Body()
		ehttptest.Eq(t, "io.ReadAll", err, nil)
		ehttptest.Eq(t, "io.ReadAll", string(b), "abc")

		s, err := aReq("POST", "/", strings.NewReader("abc")).BodyString()
		ehttptest.Eq(t, "io.ReadAll", err, nil)
		ehttptest.Eq(t, "io.ReadAll", s, "abc")
	})

	t.Run("large body without a limit is read whole", func(t *testing.T) {
		t.Parallel()

		b, err := aReq("POST", "/", strings.NewReader(strings.Repeat("x", 1<<20))).Body()
		ehttptest.Eq(t, "io.ReadAll", err, nil)
		ehttptest.Eq(t, "io.ReadAll", len(b), 1<<20)
	})

	t.Run("body is single-read", func(t *testing.T) {
		t.Parallel()

		req := aReq("POST", "/", strings.NewReader("abc"))
		_, _ = req.Body()
		b, err := req.Body()
		ehttptest.Eq(t, "net/http: Request.Body is a one-shot stream", err, nil)
		ehttptest.Eq(t, "net/http: Request.Body is a one-shot stream", len(b), 0)
	})

	t.Run("over the limit maps to ErrRequestBodyTooLarge", func(t *testing.T) {
		t.Parallel()

		limited := func() *Request {
			req := aReq("POST", "/", strings.NewReader("hello world"))
			req.Request.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Request.Body, 4)

			return req
		}

		_, err := limited().Body()
		ehttptest.True(t, "ErrRequestBodyTooLarge contract", errors.Is(err, ErrRequestBodyTooLarge), "Body: got %v", err)

		_, err = limited().BodyString()
		ehttptest.True(t, "ErrRequestBodyTooLarge contract", errors.Is(err, ErrRequestBodyTooLarge), "BodyString: got %v", err)

		var v any

		err = limited().BodyJSON(&v)
		ehttptest.True(t, "ErrRequestBodyTooLarge contract", errors.Is(err, ErrRequestBodyTooLarge), "BodyJSON: got %v", err)
	})

	t.Run("read error is returned as is", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("boom")
		mk := func() *Request { return aReq("POST", "/", iotest.ErrReader(boom)) }

		_, err := mk().Body()
		ehttptest.True(t, "io.ReadAll", errors.Is(err, boom) && !errors.Is(err, ErrRequestBodyTooLarge), "Body: got %v", err)

		_, err = mk().BodyString()
		ehttptest.True(t, "io.ReadAll", errors.Is(err, boom) && !errors.Is(err, ErrRequestBodyTooLarge), "BodyString: got %v", err)

		var v any

		err = mk().BodyJSON(&v)
		ehttptest.True(t, "io.ReadAll", errors.Is(err, boom) && !errors.Is(err, ErrRequestBodyTooLarge), "BodyJSON: got %v", err)
	})
}

func TestRequestBodyJSON(t *testing.T) {
	t.Parallel()

	type payload struct {
		A int `json:"a"`
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		var p payload

		ehttptest.Eq(t, "RFC 8259", aReq("POST", "/", strings.NewReader(`{"a":7}`)).BodyJSON(&p), nil)
		ehttptest.Eq(t, "RFC 8259", p.A, 7)
	})

	t.Run("null leaves the target untouched", func(t *testing.T) {
		t.Parallel()

		p := payload{A: 1}

		ehttptest.Eq(t, "encoding/json: null is a no-op", aReq("POST", "/", strings.NewReader(`null`)).BodyJSON(&p), nil)
		ehttptest.Eq(t, "encoding/json: null is a no-op", p.A, 1)
	})

	// C13 is an open decision: these document the behavior today.
	for _, tt := range []struct {
		name, body string
	}{
		{"empty body is an error today", ""},
		{"whitespace-only body is an error today", "  \n"},
		{"malformed", `{"a":`},
		{"trailing garbage", `{"a":1}xyz`},
		{"wrong type", `{"a":"x"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var p payload

			err := aReq("POST", "/", strings.NewReader(tt.body)).BodyJSON(&p)
			ehttptest.True(t, "C13 (current behavior documented)", err != nil, "expected an error for %q", tt.body)
		})
	}

	t.Run("io.EOF check is dead code", func(t *testing.T) {
		t.Parallel()

		var p payload

		err := aReq("POST", "/", nil).BodyJSON(&p)
		ehttptest.True(t, "C13: json.Unmarshal never returns io.EOF", err != nil && !errors.Is(err, io.EOF), "got %v", err)
	})

	t.Run("non-pointer target", func(t *testing.T) {
		t.Parallel()

		var p payload

		err := aReq("POST", "/", strings.NewReader(`{}`)).BodyJSON(p)
		ehttptest.True(t, "encoding/json InvalidUnmarshalError", err != nil, "expected an error")
	})
}

func TestRequestProtocolAndHost(t *testing.T) {
	t.Parallel()

	t.Run("absolute-form target carries its scheme", func(t *testing.T) {
		t.Parallel()

		req := aReq("GET", "http://example.com/x", nil)
		ehttptest.Eq(t, "RFC 9112 §3.2.2", req.Protocol(), "http")
		ehttptest.Eq(t, "RFC 9112 §3.2.2", req.Hostname(), "example.com")
	})

	t.Run("Host without forwarding headers is the Host header", func(t *testing.T) {
		t.Parallel()

		req := aReq("GET", "/", nil)
		req.Request.Host = "example.com:8080"
		ehttptest.Eq(t, "net/http Request.Host", req.Host(), "example.com:8080")
	})

	t.Run("plain HTTP request has protocol http", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: Protocol() derives from r.TLS", aReq("GET", "/", nil).Protocol(), "http")
		})
	})

	t.Run("TLS request has protocol https", func(t *testing.T) {
		t.Parallel()

		req := aReq("GET", "/", nil)
		req.Request.TLS = &tls.ConnectionState{}

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: Protocol() derives from r.TLS", req.Protocol(), "https")
		})
	})

	t.Run("Hostname is the Host header without the port", func(t *testing.T) {
		t.Parallel()

		req := aReq("GET", "/", nil)
		req.Request.Host = "example.com:8080"

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: Hostname() comes from r.Host without the port", req.Hostname(), "example.com")
		})
	})

	for _, h := range []string{HeaderXForwardedProto, HeaderXForwardedProtocol} {
		t.Run("untrusted "+h+" is ignored", func(t *testing.T) {
			t.Parallel()

			req := aReq("GET", "/", nil, h, "https")
			req.Request.TLS = nil

			ehttptest.KnownBug(t, "S7", func(t testing.TB) {
				ehttptest.Eq(t, "S7: X-Forwarded-* only from trusted proxies (Express, Echo v5, Fiber v3)", req.Protocol(), "http")
			})
		})
	}

	t.Run("untrusted X-Forwarded-Host is ignored", func(t *testing.T) {
		t.Parallel()

		req := aReq("GET", "/", nil, HeaderXForwardedHost, "evil.example")

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: X-Forwarded-* only from trusted proxies", req.Host(), "example.com")
		})
	})
}

// Premise of S7 checked against a real net/http server: server-side
// requests have an empty URL.Scheme and URL.Host.
func TestRequestProtocolAndHostOverRealServer(t *testing.T) {
	t.Parallel()

	handler := func() http.Handler {
		s := NewServer()
		s.GET("/", func(res *ResponseWriter, req *Request) {
			res.String(req.Protocol() + "|" + req.Hostname())
		})

		return s
	}

	t.Run("http", func(t *testing.T) {
		t.Parallel()

		ts := aRealServer(t, handler())
		_, body := aGetBody(t, ts.URL+"/")

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: Protocol/Hostname work on a real server", body, "http|127.0.0.1")
		})
	})

	t.Run("https", func(t *testing.T) {
		t.Parallel()

		ts := httptest.NewUnstartedServer(handler())
		ts.StartTLS()
		t.Cleanup(ts.Close)

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/", http.NoBody)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)

		ehttptest.KnownBug(t, "S7", func(t testing.TB) {
			ehttptest.Eq(t, "S7: Protocol() derives from r.TLS", string(b), "https|127.0.0.1")
		})
	})
}
