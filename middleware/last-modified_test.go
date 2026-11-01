package middleware_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

var bModified = time.Date(2024, 5, 6, 7, 8, 9, 500, time.UTC)

const bModifiedHeader = "Mon, 06 May 2024 07:08:09 GMT"

func TestLastModified(t *testing.T) {
	t.Parallel()

	earlier := bModified.Add(-time.Hour).Format(http.TimeFormat)
	later := bModified.Add(time.Hour).Format(http.TimeFormat)

	tests := []struct {
		name       string
		method     string
		ims        string
		wantCode   int
		wantNext   bool
		wantHeader bool
	}{
		{"GET without IMS", "GET", "", 200, true, true},
		{"HEAD without IMS", "HEAD", "", 200, true, true},
		{"GET with older IMS is modified", "GET", earlier, 200, true, true},
		{"GET with equal IMS is not modified", "GET", bModifiedHeader, 304, false, true},
		{"GET with later IMS is not modified", "GET", later, 304, false, true},
		{"invalid IMS is ignored", "GET", "yesterday", 200, true, true},
		{"IMS on POST is ignored and no header", "POST", later, 200, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			called := false
			h := func(res *ehttp.ResponseWriter, req *ehttp.Request) {
				called = true
				res.String("ok")
			}

			var handler http.Handler

			if tt.method == "POST" {
				handler = fixtures.PostHandler(h, middleware.LastModified(bModified))
			} else {
				handler = fixtures.Handler(h, middleware.LastModified(bModified))
			}

			rec := ehttptest.Do(handler, fixtures.Req(tt.method, "/x", "If-Modified-Since", tt.ims))

			ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, tt.wantCode)
			ehttptest.Eq(t, "middleware contract (next called)", called, tt.wantNext)

			if tt.wantHeader {
				ehttptest.Eq(t, "RFC 9110 §8.8.2 (second precision, IMF-fixdate)", rec.Header().Get("Last-Modified"), bModifiedHeader)
			} else {
				ehttptest.Eq(t, "RFC 9110 §8.8.2", rec.Header().Get("Last-Modified"), "")
			}
		})
	}

	t.Run("sub-second part is truncated so a client echoing the header gets 304", func(t *testing.T) {
		t.Parallel()

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.LastModified(bModified)), fixtures.Req("GET", "/x", "If-Modified-Since", bModifiedHeader))

		ehttptest.Eq(t, "RFC 9110 §8.8.2.1", rec.Code, 304)
	})

	t.Run("non-UTC input is normalised to GMT", func(t *testing.T) {
		t.Parallel()

		zone := time.FixedZone("X", 3*3600)
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.LastModified(bModified.In(zone))), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "RFC 9110 §5.6.7", rec.Header().Get("Last-Modified"), bModifiedHeader)
	})
}

// RFC 9110 §13.1.3: a recipient MUST ignore If-Modified-Since when the
// request has If-None-Match.
func TestLastModifiedIgnoredWithIfNoneMatch(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "NB-3", func(t testing.TB) {
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.LastModified(bModified)),
			fixtures.Req("GET", "/x", "If-None-Match", `"no-match"`, "If-Modified-Since", bModifiedHeader))

		ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 200)
	})
}

// RFC 9110 §5.6.7: recipients MUST accept the obsolete RFC 850 and asctime
// date formats; http.ParseTime does, time.Parse(http.TimeFormat) does not.
func TestLastModifiedObsoleteDateFormats(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"RFC 850": "Monday, 06-May-24 08:08:09 GMT",
		"asctime": "Mon May  6 08:08:09 2024",
	}

	for name, ims := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "NB-4", func(t testing.TB) {
				rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.LastModified(bModified)), fixtures.Req("GET", "/x", "If-Modified-Since", ims))

				ehttptest.Eq(t, "RFC 9110 §5.6.7", rec.Code, 304)
			})
		})
	}
}
