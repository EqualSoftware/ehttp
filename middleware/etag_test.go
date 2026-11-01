package middleware_test

import (
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			{"garbage", "nope", 200},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x", "If-None-Match", tt.inm))

				ehttptest.Eq(t, "RFC 9110 §13.1.2", rec.Code, tt.want)
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

// C3: If-None-Match is a list or "*" (RFC 9110 §13.1.2). The middleware only
// compares the whole header with a single tag.
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

			ehttptest.KnownBug(t, "C3", func(t testing.TB) {
				rec := ehttptest.Do(fixtures.Handler(bString(body), middleware.WeakEtag()), fixtures.Req("GET", "/x", "If-None-Match", tt.inm))

				ehttptest.Eq(t, "RFC 9110 §13.1.2", rec.Code, 304)
			})
		})
	}
}

// C3: 413 is for request bodies (RFC 9110 §15.5.14); a large response is not
// an error. The size is faked through the exported interceptor field.
func TestWeakEtagLargeResponseIsNot413(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "C3", func(t testing.TB) {
		h := fixtures.Handler(func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			res.String("x")
			res.Writer.(*ehttp.ResponseWriterInterceptor).Size = math.MaxUint32 + 1
		}, middleware.WeakEtag())

		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.True(t, "RFC 9110 §15.5.14", rec.Code != 413, "response of >4GiB was answered 413")
		ehttptest.Eq(t, "middleware contract", rec.Code, 200)
	})
}

// Composition with ErrorCache, in both orders (C4 aside, the pair must work).
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
