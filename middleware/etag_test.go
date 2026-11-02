package middleware_test

import (
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func bETag(body string) string {
	return fmt.Sprintf(`W/"%d-%08x"`, len(body), crc32.ChecksumIEEE([]byte(body)))
}

func bString(body string) ehttp.HandlerFunc {
	return func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.String(body) }
}

func TestWeakEtag(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	t.Run("GET 200 gets a weak validator over the body", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "RFC 9110 §8.8.3", rec.Code, 200)
		ehttptest.Eq(t, "RFC 9110 §8.8.3", rec.Header().Get("ETag"), bETag(body))
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), body)
	})

	t.Run("same body same etag, other body other etag", func(t *testing.T) {
		t.Parallel()

		a := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x")).Header().Get("ETag")
		b := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x")).Header().Get("ETag")
		c := ehttptest.Do(fixtures.Handler(bString(body+"!"), middleware.WeakEtag()), fixtures.Req("GET", "/x")).Header().Get("ETag")

		ehttptest.Eq(t, "RFC 9110 §8.8.3", a, b)
		ehttptest.True(t, "RFC 9110 §8.8.3", a != c, "different bodies share the etag %q", a)
	})

	t.Run("If-None-Match", func(t *testing.T) {
		t.Parallel()

		etag := bETag(body)
		tests := []struct {
			name string
			inm  string
			want int
		}{
			{"weak match", etag, 304},
			{"strong form of the same tag (weak comparison)", etag[2:], 304},
			{"different tag", `W/"1-00000000"`, 200},
			{"garbage is an invalid syntax", "nope", 400},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x", "If-None-Match", tt.inm))

				ehttptest.Eq(t, "RFC 9110 §13.1.2", rec.Code, tt.want)

				if tt.want == 400 {
					return
				}

				ehttptest.Eq(t, "RFC 9110 §15.4.5 (304 carries ETag)", rec.Header().Get("ETag"), etag)

				if tt.want == 304 {
					ehttptest.Eq(t, "RFC 9110 §15.4.5 (304 has no content)", rec.Body.Len(), 0)
				}
			})
		}
	})

	t.Run("methods other than GET and HEAD are untouched", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.PostHandler(bString(body), middleware.WeakEtag()), fixtures.Req("POST", "/x", "If-None-Match", bETag(body)))

		ehttptest.Eq(t, "RFC 9110 §13.1.2 (If-None-Match on POST precondition is not this middleware's job)", rec.Code, 200)
		ehttptest.Eq(t, "middleware contract", rec.Header().Get("ETag"), "")
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), body)
	})

	t.Run("non-200 and empty responses get no etag and pass through", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.Status(404, "missing"), middleware.WeakEtag()), fixtures.Req("GET", "/x"))
		ehttptest.Eq(t, "RFC 9110 §8.8.3", rec.Code, 404)
		ehttptest.Eq(t, "RFC 9110 §8.8.3", rec.Header().Get("ETag"), "")
		ehttptest.Eq(t, "middleware contract", rec.Body.String(), "missing")

		rec = ehttptest.Do(fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) { res.SendStatus(200) }, middleware.WeakEtag()), fixtures.Req("GET", "/x"))
		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
		ehttptest.Eq(t, "middleware contract", rec.Header().Get("ETag"), "")
	})

	t.Run("calls next exactly once even on 304", func(t *testing.T) {
		t.Parallel()

		calls := 0
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			calls++
			res.String(body)
		}, middleware.WeakEtag())

		ehttptest.Do(h, fixtures.Req("GET", "/x", "If-None-Match", bETag(body)))

		ehttptest.Eq(t, "middleware contract", calls, 1)
	})
}

// HEAD must report the same headers as GET (RFC 9110 §9.3.2) and no body.
// ResponseRecorder keeps HEAD bodies, so this runs over a real server.
func TestWeakEtagHead(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	srv := httptest.NewServer(fixtures.Handler(bString(body), middleware.WeakEtag()))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	b, _ := io.ReadAll(res.Body)

	ehttptest.Eq(t, "RFC 9110 §9.3.2", res.StatusCode, 200)
	ehttptest.Eq(t, "RFC 9110 §9.3.2", res.Header.Get("ETag"), bETag(body))
	ehttptest.Eq(t, "RFC 9110 §9.3.2", res.Header.Get("Content-Length"), strconv.Itoa(len(body)))
	ehttptest.Eq(t, "RFC 9110 §9.3.2", len(b), 0)
}

// If-None-Match is a list or "*" (RFC 9110 §13.1.2).
func TestWeakEtagIfNoneMatchList(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	etag := bETag(body)
	tests := []struct{ name, inm string }{
		{"list with the matching tag last", `"zzz", ` + etag},
		{"list with the matching tag first", etag + `, "zzz"`},
		{"list without spaces", `"zzz",` + etag},
		{"wildcard", "*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x", "If-None-Match", tt.inm))

			ehttptest.Eq(t, "RFC 9110 §13.1.2", rec.Code, 304)
		})
	}
}

// 413 is for request bodies (RFC 9110 §15.5.14); a large response is not an
// error. The middleware buffers up to 1 MiB for the ETag. A larger response is
// streamed without an ETag, and the full body arrives.
func TestWeakEtagLargeResponse(t *testing.T) {
	t.Parallel()

	const limit = 1 << 20

	tests := []struct {
		name     string
		size     int
		wantEtag bool
	}{
		{"at the limit gets an ETag", limit, true},
		{"over the limit is streamed", limit + 1, false},
		{"far over the limit is streamed", 5 * limit, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := strings.Repeat("x", tt.size)
			ts := httptest.NewServer(fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
				// Two writes: the second one crosses the limit.
				_, _ = res.Write([]byte(body[:limit/2]))
				_, _ = res.Write([]byte(body[limit/2:]))
			}, middleware.WeakEtag()))
			defer ts.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			defer resp.Body.Close()

			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			ehttptest.Eq(t, "RFC 9110 §15.5.14", resp.StatusCode, 200)
			ehttptest.Eq(t, "middleware contract", len(got), tt.size)
			ehttptest.Eq(t, "RFC 9110 §8.8.3", resp.Header.Get("ETag") != "", tt.wantEtag)
		})
	}
}

// A handler that flushes cannot wait for the whole body: the bytes before the
// flush reach the client while the handler still runs, and there is no ETag.
func TestWeakEtagFlushStreams(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	ts := httptest.NewServer(fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		_, _ = res.Write([]byte("first"))
		res.Flush()
		<-release
		_, _ = res.Write([]byte("second"))
	}, middleware.WeakEtag()))
	defer ts.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	ehttptest.Eq(t, "RFC 9110 §8.8.3", resp.Header.Get("ETag"), "")

	first := make([]byte, len("first"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}

	ehttptest.Eq(t, "middleware contract (the chunk arrives before the handler ends)", string(first), "first")

	close(release)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	ehttptest.Eq(t, "middleware contract", string(rest), "second")
}

// A flush under WeakEtag commits the response without an ETag: it works.
func TestFlushInsideWeakEtagWorks(t *testing.T) {
	t.Parallel()

	var got error

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		got = http.NewResponseController(res).Flush()
		res.String("x")
	}, middleware.WeakEtag())

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "ResponseController.Flush", got, nil)
	ehttptest.Eq(t, "body still sent once", rec.Body.String(), "x")
}

// Composition with ErrorCache, in both orders (the pair must work).
func TestWeakEtagWithErrorCache(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	orders := map[string][]ehttp.MiddlewareHandlerFunc{
		"etag outside":        {middleware.WeakEtag(), middleware.ErrorCache(time.Minute)},
		"error cache outside": {middleware.ErrorCache(time.Minute), middleware.WeakEtag()},
	}

	for name, mws := range orders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ok := ehttptest.Do(fixtures.Handler(bString(body), mws...), fixtures.Req("GET", "/x"))
			ehttptest.Eq(t, "composition", ok.Code, 200)
			ehttptest.Eq(t, "composition", ok.Header().Get("ETag"), bETag(body))
			ehttptest.Eq(t, "composition", ok.Header().Get("Cache-Control"), "")
			ehttptest.Eq(t, "composition", ok.Body.String(), body)

			nm := ehttptest.Do(fixtures.Handler(bString(body), mws...), fixtures.Req("GET", "/x", "If-None-Match", bETag(body)))
			ehttptest.Eq(t, "RFC 9110 §13.1.2", nm.Code, 304)
			ehttptest.Eq(t, "composition", nm.Body.Len(), 0)

			bad := ehttptest.Do(fixtures.Handler(fixtures.Status(500, "err"), mws...), fixtures.Req("GET", "/x"))
			ehttptest.Eq(t, "composition", bad.Code, 500)
			ehttptest.Eq(t, "composition", bad.Body.String(), "err")
			ehttptest.Eq(t, "composition", bad.Header().Get("ETag"), "")
			ehttptest.True(t, "composition", bad.Header().Get("Cache-Control") != "", "error response lost Cache-Control")
		})
	}
}

// A native handler behind WrapHandler is intercepted and sent one time.
func TestWrapHandlerInsideInterceptors(t *testing.T) {
	t.Parallel()

	native := ehttp.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))

	for name, mw := range map[string]ehttp.MiddlewareHandlerFunc{
		"ErrorCache": middleware.ErrorCache(time.Minute),
		"WeakEtag":   middleware.WeakEtag(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(native, mw), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "intercepted status", rec.Code, 404)
			ehttptest.Eq(t, "intercepted body, once", rec.Body.String(), "nope")
		})
	}
}

// SendFile answers its own conditionals and ranges. When WeakEtag or
// LastModified is also active, the response has one decision and a 304 has no
// content. The check runs on a real connection, and a second request on the
// same connection proves that the first one ended clean.
func TestSendFileWithValidatorMiddleware(t *testing.T) {
	t.Parallel()

	name := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(name, []byte(strings.Repeat("0123456789", 100)), 0o600); err != nil {
		t.Fatal(err)
	}

	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	send := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendFile(name) }
	servers := map[string]http.Handler{
		"WeakEtag":     fixtures.Handler(send, middleware.WeakEtag()),
		"LastModified": fixtures.Handler(send, middleware.LastModified(modified)),
	}

	doMethod := func(t *testing.T, c *http.Client, method, url string, kv ...string) (*http.Response, string) {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < len(kv); i += 2 {
			req.Header.Set(kv[i], kv[i+1])
		}

		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		return resp, string(body)
	}

	do := func(t *testing.T, c *http.Client, url string, kv ...string) (*http.Response, string) {
		t.Helper()

		return doMethod(t, c, http.MethodGet, url, kv...)
	}

	for label, h := range servers {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			ts := httptest.NewServer(h)
			defer ts.Close()

			url := ts.URL + "/x"
			c := ts.Client()
			first, full := do(t, c, url)
			etag := first.Header.Get("ETag")

			ehttptest.Eq(t, "RFC 9110 §15.3.1", first.StatusCode, 200)
			ehttptest.Eq(t, "RFC 9110 §15.3.1", len(full), 1000)
			ehttptest.True(t, "RFC 9110 §8.8.3", etag != "", "a 200 has no ETag")

			// The validator of the response gives one 304 without content.
			nm, body := do(t, c, url, "If-None-Match", etag)
			ehttptest.Eq(t, "RFC 9110 §13.1.2", nm.StatusCode, 304)
			ehttptest.Eq(t, "RFC 9110 §15.4.5", body, "")

			// A date of the file gives one 304 without content.
			ims, body := do(t, c, url, "If-Modified-Since", time.Now().UTC().Format(http.TimeFormat))
			ehttptest.Eq(t, "RFC 9110 §13.1.3", ims.StatusCode, 304)
			ehttptest.Eq(t, "RFC 9110 §15.4.5", body, "")

			// A precondition that fails is 412, never turned into 304.
			pre, _ := do(t, c, url, "If-Match", `"nope"`, "If-Modified-Since", time.Now().UTC().Format(http.TimeFormat))
			ehttptest.Eq(t, "RFC 9110 §13.2.1", pre.StatusCode, 412)

			// A range is a 206 with the bytes of the range.
			rng, part := do(t, c, url, "Range", "bytes=0-9")
			ehttptest.Eq(t, "RFC 9110 §14.4", rng.StatusCode, 206)
			ehttptest.Eq(t, "RFC 9110 §14.4", part, "0123456789")

			// One representation has one ETag (RFC 9110 §15.3.7).
			ehttptest.Eq(t, "RFC 9110 §15.3.7", rng.Header.Get("ETag"), etag)

			// HEAD keeps the real length and the ETag of the 200.
			head, hbody := doMethod(t, c, http.MethodHead, url)
			ehttptest.Eq(t, "RFC 9110 §9.3.2", head.StatusCode, 200)
			ehttptest.Eq(t, "RFC 9110 §9.3.2", head.Header.Get("Content-Length"), "1000")
			ehttptest.Eq(t, "RFC 9110 §9.3.2", head.Header.Get("ETag"), etag)
			ehttptest.Eq(t, "RFC 9110 §9.3.2", hbody, "")
		})
	}
}

// If-Match uses the strong comparison (RFC 9110 section 13.1.1). The ETag of
// WeakEtag is weak, so only "*" passes, and any tag list gets 412 before
// If-None-Match is read (section 13.2.2).
func TestWeakEtagIfMatch(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	weak := bETag(body)
	strong := strings.TrimPrefix(weak, "W/")
	tests := []struct {
		name, im string
		code     int
	}{
		{"weak tag of the response", weak, 412},
		{"strong form of the tag", strong, 412},
		{"list with the weak tag", `"zzz", ` + weak, 412},
		{"other tag", `"nope"`, 412},
		{"wildcard", "*", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x", "If-Match", tt.im))

			ehttptest.Eq(t, "RFC 9110 §13.1.1", rec.Code, tt.code)

			if tt.code == 412 {
				ehttptest.Eq(t, "RFC 9110 §15.5.13", rec.Body.String(), "")
			}
		})
	}

	t.Run("If-Match fails before If-None-Match", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()),
			fixtures.Req("GET", "/x", "If-Match", `"nope"`, "If-None-Match", weak))

		ehttptest.Eq(t, "RFC 9110 §13.2.2", rec.Code, 412)
	})
}

// SendFile sets a weak ETag, so a tag list of If-Match never matches it
// (strong comparison, RFC 9110 section 13.1.1) and "*" does.
func TestSendFileIfMatchStrong(t *testing.T) {
	t.Parallel()

	name := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(name, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := fixtures.Handler(func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendFile(name) })
	etag := ehttptest.Do(h, fixtures.Req("GET", "/x")).Header().Get("ETag")

	ehttptest.True(t, "RFC 9110 §8.8.3", etag != "", "the response has no ETag")

	tests := []struct {
		name, im string
		code     int
	}{
		{"weak tag of the response", etag, 412},
		{"strong form of the tag", strings.TrimPrefix(etag, "W/"), 412},
		{"list with the tag", `"zzz", ` + etag, 412},
		{"wildcard", "*", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "If-Match", tt.im))

			ehttptest.Eq(t, "RFC 9110 §13.1.1", rec.Code, tt.code)
		})
	}
}

// The 412 of If-Match drops the headers of the discarded body. On a real
// server the client must read the full response (a kept Content-Length with
// no body gives an unexpected EOF).
func TestWeakEtagIfMatchRealServer(t *testing.T) {
	t.Parallel()

	empty200 := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(200) }
	no204 := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendStatus(204) }

	tests := []struct {
		name, im string
		h        ehttp.HandlerFunc
		code     int
	}{
		{"body, tag list", `"nope"`, bString("hello world"), 412},
		{"body, wildcard", "*", bString("hello world"), 200},
		{"empty 200, tag list", `"nope"`, empty200, 412},
		{"empty 200, wildcard", "*", empty200, 200},
		{"204, tag list", `"nope"`, no204, 412},
		{"204, wildcard", "*", no204, 204},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := httptest.NewServer(fixtures.Handler(tt.h, middleware.WeakEtag()))
			defer ts.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("If-Match", tt.im)

			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)

			ehttptest.Eq(t, "RFC 9110 §13.2.2", resp.StatusCode, tt.code)
			ehttptest.True(t, "the response is read in full", err == nil, "read: %v", err)

			if tt.code == 412 {
				ehttptest.Eq(t, "RFC 9110 §15.5.13", len(body), 0)
				ehttptest.Eq(t, "the dropped body has no ETag", resp.Header.Get("ETag"), "")
			}
		})
	}
}
