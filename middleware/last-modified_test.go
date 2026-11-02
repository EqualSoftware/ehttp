package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
	"go.uber.org/zap/zaptest/observer"
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
		{"GET with equal IMS is not modified", "GET", bModifiedHeader, 304, true, true},
		{"GET with later IMS is not modified", "GET", later, 304, true, true},
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
			ehttptest.Eq(t, "middleware contract (the handler runs to give the status, RFC 9110 §13.2.1)", called, tt.wantNext)

			if tt.wantCode == 304 {
				ehttptest.Eq(t, "RFC 9110 §15.4.5 (a 304 has no content)", rec.Body.Len(), 0)
			}

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

// lmDo serves GET /x through LastModified and returns the recorded response.
// kv is a flat list of request header name/value pairs.
func lmDo(h ehttp.HandlerFunc, kv ...string) *httptest.ResponseRecorder {
	return ehttptest.Do(fixtures.Handler(h, middleware.LastModified(bModified)), fixtures.Req("GET", "/x", kv...))
}

// RFC 9110 §13.1.3: a recipient MUST ignore If-Modified-Since when the
// request has If-None-Match.
func TestLastModifiedIgnoredWithIfNoneMatch(t *testing.T) {
	t.Parallel()

	rec := lmDo(fixtures.OK, "If-None-Match", `"no-match"`, "If-Modified-Since", bModifiedHeader)

	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 200)
	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Body.String(), "ok")
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

			ehttptest.Eq(t, "RFC 9110 §5.6.7", lmDo(fixtures.OK, "If-Modified-Since", ims).Code, 304)
		})
	}
}

// RFC 9110 §13.1.3: a recipient MUST ignore If-Modified-Since when the field
// has more than one member. IMF-fixdate has a comma, so one date is not a list.
func TestLastModifiedMultipleMembers(t *testing.T) {
	t.Parallel()

	t.Run("comma-separated list of dates", func(t *testing.T) {
		t.Parallel()

		ehttptest.Eq(t, "RFC 9110 §13.1.3", lmDo(fixtures.OK, "If-Modified-Since", bModifiedHeader+", "+bModifiedHeader).Code, 200)
	})

	t.Run("two field lines", func(t *testing.T) {
		t.Parallel()

		req := fixtures.Req("GET", "/x")
		req.Header.Add("If-Modified-Since", bModifiedHeader)
		req.Header.Add("If-Modified-Since", bModifiedHeader)

		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.LastModified(bModified)), req)

		ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 200)
	})

	t.Run("one date with its comma is not a list", func(t *testing.T) {
		t.Parallel()

		ehttptest.Eq(t, "RFC 9110 §5.6.7", lmDo(fixtures.OK, "If-Modified-Since", bModifiedHeader).Code, 304)
	})
}

// RFC 9110 §13.2.1: a server MUST ignore the preconditions when the response
// without them is not 2xx (or 412). A 304 must not hide a 404.
func TestLastModifiedIgnoredForNon2xx(t *testing.T) {
	t.Parallel()

	for _, code := range []int{404, 500, 301} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()

			rec := lmDo(fixtures.Status(code, "body"), "If-Modified-Since", bModifiedHeader)

			ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, code)
			ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Body.String(), "body")
		})
	}

	t.Run("a 2xx other than 200 can be 304", func(t *testing.T) {
		t.Parallel()

		rec := lmDo(fixtures.Status(203, "body"), "If-Modified-Since", bModifiedHeader)

		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 304)
	})
}

// lmServe serves GET or HEAD /x through LastModified in Development mode, where
// a strict error panics, with a conditional request that says "not modified".
// It returns the response and the log entries.
func lmServe(method string, h ehttp.HandlerFunc) (*httptest.ResponseRecorder, *observer.ObservedLogs) {
	l, logs := fixtures.ObservedLogger()
	s := ehttp.NewServer(ehttp.WithLogger(l), ehttp.WithMode(ehttp.Development))
	s.GET("/x", h, middleware.LastModified(bModified))

	return ehttptest.Do(s, fixtures.Req(method, "/x", "If-Modified-Since", bModifiedHeader)), logs
}

// RFC 9110 §15.4.5: a 304 keeps the fields that a 200 would have (Cache-Control,
// Content-Location, ETag, Expires, Vary, Last-Modified) and has no content, so
// no Content-Length, no Content-Type and no body.
func TestLastModified304Headers(t *testing.T) {
	t.Parallel()

	rec, _ := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Set("Cache-Control", "max-age=60")
		res.Set("Content-Location", "/x.html")
		res.Set("ETag", `"v1"`)
		res.Set("Expires", "Mon, 06 May 2024 09:00:00 GMT")
		res.Vary("Accept")
		res.String("the body")
	})

	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 304)
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no content)", rec.Body.Len(), 0)

	for name, want := range map[string]string{
		"Cache-Control": "max-age=60", "Content-Location": "/x.html", "ETag": `"v1"`,
		"Expires": "Mon, 06 May 2024 09:00:00 GMT", "Vary": "Accept", "Last-Modified": bModifiedHeader,
	} {
		ehttptest.Eq(t, "RFC 9110 §15.4.5 header "+name, rec.Header().Get(name), want)
	}

	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no Content-Length)", rec.Header().Get("Content-Length"), "")
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no Content-Type)", rec.Header().Get("Content-Type"), "")
}

// RFC 9110 §15.3.1: the first write without a status is an implicit 200, which
// is a 2xx response, so the condition gives 304.
func TestLastModified304ImplicitStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]ehttp.HandlerFunc{
		"first Write":     func(res *ehttp.ResponseWriter, _ *ehttp.Request) { _, _ = res.Write([]byte("body")) },
		"nothing written": func(*ehttp.ResponseWriter, *ehttp.Request) {},
		"status 200 only": func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.Status(200) },
		"status 206":      func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.Status(206); res.String("part") },
	}

	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec, _ := lmServe("GET", h)

			ehttptest.Eq(t, "RFC 9110 §13.2.1 (2xx without the condition)", rec.Code, 304)
			ehttptest.Eq(t, "RFC 9110 §15.4.5", rec.Body.Len(), 0)
		})
	}
}

// The handler does not know about the condition. Its body writes after the 304
// are dropped without a strict error (a panic in Development, a warning in
// Production), and Flush after the 304 does nothing.
func TestLastModified304DropsStreamedBody(t *testing.T) {
	t.Parallel()

	chunk := []byte(strings.Repeat("x", 32<<10))

	rec, logs := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Status(200)

		for range 8 {
			_, _ = res.Write(chunk)
			res.Flush()
		}
	})

	ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 304)
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (the body is dropped)", rec.Body.Len(), 0)
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no strict warning)", logs.Len(), 0)
	ehttptest.True(t, "Flush after the 304 is a no-op", !rec.Flushed, "the recorder was flushed")
}

// RFC 9110 §9.3.2: HEAD has the same status and fields as GET.
func TestLastModified304Head(t *testing.T) {
	t.Parallel()

	rec, _ := lmServe("HEAD", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("body") })

	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 304)
	ehttptest.Eq(t, "RFC 9110 §15.4.5", rec.Body.Len(), 0)
	ehttptest.Eq(t, "RFC 9110 §8.8.2", rec.Header().Get("Last-Modified"), bModifiedHeader)
}

// RFC 9110 §13.2.1: a status other than 2xx passes through with its headers
// and its body, streamed, and Flush works.
func TestLastModifiedNon2xxStreams(t *testing.T) {
	t.Parallel()

	rec, logs := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Status(404)
		res.Set("X-Reason", "gone")
		_, _ = res.Write([]byte("part1,"))
		res.Flush()
		_, _ = res.Write([]byte("part2"))
	})

	ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 404)
	ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Body.String(), "part1,part2")
	ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Header().Get("X-Reason"), "gone")
	ehttptest.True(t, "Flush passes through a non-2xx response", rec.Flushed, "the recorder was not flushed")
	ehttptest.Eq(t, "no strict warning", logs.Len(), 0)
}

// countReader gives n bytes and counts its Read calls.
type countReader struct{ reads, left int }

func (c *countReader) Read(p []byte) (int, error) {
	c.reads++

	if c.left == 0 {
		return 0, io.EOF
	}

	n := min(len(p), c.left)
	c.left -= n

	return n, nil
}

// RFC 9110 §15.4.5: a 304 has no content, so a copy from a reader (io.Copy,
// SendFile) must not read its source after the 304.
func TestLastModified304ReadsNoSource(t *testing.T) {
	t.Parallel()

	t.Run("io.Copy", func(t *testing.T) {
		t.Parallel()

		src := &countReader{left: 1 << 20}
		rec, _ := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { _, _ = io.Copy(res, src) })

		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 304)
		ehttptest.Eq(t, "RFC 9110 §15.4.5 (the source is not read)", src.reads, 0)
		ehttptest.Eq(t, "RFC 9110 §15.4.5", rec.Body.Len(), 0)
	})

	t.Run("SendFile", func(t *testing.T) {
		t.Parallel()

		name := filepath.Join(t.TempDir(), "f.txt")
		if err := os.WriteFile(name, []byte(strings.Repeat("x", 1<<16)), 0o600); err != nil {
			t.Fatal(err)
		}

		rec, logs := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.SendFile(name) })

		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 304)
		ehttptest.Eq(t, "RFC 9110 §15.4.5", rec.Body.Len(), 0)
		ehttptest.Eq(t, "no strict warning", logs.Len(), 0)
	})

	t.Run("a non-2xx status copies the source", func(t *testing.T) {
		t.Parallel()

		src := &countReader{left: 10}
		rec, _ := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.Status(500); _, _ = io.Copy(res, src) })

		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Code, 500)
		ehttptest.Eq(t, "RFC 9110 §13.2.1", rec.Body.Len(), 10)
	})
}

// RFC 9110 §15.4.5: a 304 has no Content-Length and no Content-Type, also when
// the handler wrote no body and the middleware sends the 304 itself.
func TestLastModified304DropsContentFieldsWithoutBody(t *testing.T) {
	t.Parallel()

	rec, _ := lmServe("GET", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Header().Set("Content-Type", "text/html")
		res.Header().Set("Content-Length", "5")
	})

	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 304)
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no Content-Type)", rec.Header().Get("Content-Type"), "")
	ehttptest.Eq(t, "RFC 9110 §15.4.5 (no Content-Length)", rec.Header().Get("Content-Length"), "")
}

// RFC 9110 §15.2.1: a 1xx response is interim; the final status still follows
// the condition. It needs a live server to send the interim response.
func TestLastModified103ThenNotModified(t *testing.T) {
	t.Parallel()

	s := ehttp.NewServer(ehttp.WithMode(ehttp.Development))
	s.GET("/x", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Header().Set("Link", "</a.css>; rel=preload")
		res.WriteHeader(103)
		res.String("body")
	}, middleware.LastModified(bModified))

	ts := fixtures.Start(t, s)

	var interim []int

	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		interim = append(interim, code)
		return nil
	}}
	ctx := httptrace.WithClientTrace(t.Context(), trace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("If-Modified-Since", bModifiedHeader)

	resp, err := fixtures.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	ehttptest.Eq(t, "RFC 9110 §15.2.1 (the interim response passes)", len(interim), 1)
	ehttptest.Eq(t, "RFC 9110 §13.1.3", resp.StatusCode, 304)
	ehttptest.Eq(t, "RFC 9110 §15.4.5", len(b), 0)
}

// A handler that panics, under Recover, gives 500, and LastModified puts the
// original writer back on the way out.
func TestLastModifiedRestoresWriterOnPanic(t *testing.T) {
	t.Parallel()

	var before, after http.ResponseWriter

	probe := func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			before = res.Unwrap()
			next.ServeHTTP(res, req)
			after = res.Unwrap()
		}
	}

	h := fixtures.Handler(func(*ehttp.ResponseWriter, *ehttp.Request) { panic("boom") },
		probe, middleware.Recover(), middleware.LastModified(bModified))

	rec := ehttptest.Do(h, fixtures.Req("GET", "/x", "If-Modified-Since", bModifiedHeader))

	ehttptest.Eq(t, "RFC 9110 §15.6.1", rec.Code, 500)
	ehttptest.True(t, "the original writer is back", before != nil && before == after, "writer before %T, after %T", before, after)
}

// notModified exposes the real writer through Unwrap, so
// http.ResponseController reaches it.
func TestLastModifiedUnwrap(t *testing.T) {
	t.Parallel()

	l, _ := fixtures.ObservedLogger()
	s := ehttp.NewServer(ehttp.WithLogger(l))

	var inner, under http.ResponseWriter

	s.GET("/x", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		inner = res.Unwrap()
		if u, ok := inner.(interface{ Unwrap() http.ResponseWriter }); ok {
			under = u.Unwrap()
		}

		res.SendStatus(200)
	}, middleware.LastModified(bModified))

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, fixtures.Req("GET", "/x", "If-Modified-Since", bModifiedHeader))

	ehttptest.True(t, "the handler sees the wrapper", inner != nil && inner != http.ResponseWriter(rec), "inner is %T", inner)
	ehttptest.True(t, "Unwrap gives the real writer", under == http.ResponseWriter(rec), "Unwrap gave %T", under)
	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 304)
}

// A middleware outside LastModified, like Log, must see the status that goes
// on the wire: 304, also when the handler wrote a 200 body (RFC 9110 §15.4.5).
func TestLastModified304StatusSeenOutside(t *testing.T) {
	t.Parallel()

	var seen int

	probe := func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			next.ServeHTTP(res, req)
			seen = res.StatusCode()
		}
	}

	serve := func(opt middleware.LogOptions) (*httptest.ResponseRecorder, []map[string]any) {
		l, logs := fixtures.ObservedLogger()
		s := ehttp.NewServer(ehttp.WithLogger(l))
		s.GET("/x", func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("body") },
			middleware.Log(opt), probe, middleware.LastModified(bModified))

		return ehttptest.Do(s, fixtures.Req("GET", "/x", "If-Modified-Since", bModifiedHeader)), bAccessLogs(logs)
	}

	rec, got := serve(middleware.LogOptions{})

	ehttptest.Eq(t, "RFC 9110 §13.1.3", rec.Code, 304)
	ehttptest.Eq(t, "StatusCode follows the wire status", seen, 304)
	ehttptest.Eq(t, "one access log line", len(got), 1)

	if len(got) == 1 {
		ehttptest.Eq(t, "Log reports 304", got[0]["code"], any(int64(304)))
	}

	_, got = serve(middleware.LogOptions{IgnoreStatus: []int{304}})

	ehttptest.Eq(t, "IgnoreStatus 304 sees the 304", len(got), 0)
}

// readerFromWriter is a response writer with a ReadFrom that counts its calls.
type readerFromWriter struct {
	*httptest.ResponseRecorder
	calls int
}

func (w *readerFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.calls++

	return io.Copy(struct{ io.Writer }{w.ResponseRecorder}, r)
}

// A pass-through status keeps the fast path (io.ReaderFrom) of the real writer.
func TestLastModifiedPassThroughUsesReadFrom(t *testing.T) {
	t.Parallel()

	l, _ := fixtures.ObservedLogger()
	s := ehttp.NewServer(ehttp.WithLogger(l))
	s.GET("/x", func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Status(500)
		_, _ = io.Copy(res, io.LimitReader(strings.NewReader("error body"), 100)) // no WriterTo, so io.Copy uses ReadFrom
	}, middleware.LastModified(bModified))

	w := &readerFromWriter{ResponseRecorder: httptest.NewRecorder()}
	s.ServeHTTP(w, fixtures.Req("GET", "/x", "If-Modified-Since", bModifiedHeader))

	ehttptest.Eq(t, "RFC 9110 §13.2.1", w.Code, 500)
	ehttptest.Eq(t, "the real ReadFrom ran one time", w.calls, 1)
	ehttptest.Eq(t, "RFC 9110 §13.2.1", w.Body.String(), "error body")

	// Live: the same handler over a real connection.
	ts := fixtures.Start(t, s)
	resp, body := fixtures.Fetch(t, http.MethodGet, ts.URL+"/x", http.Header{"If-Modified-Since": {bModifiedHeader}})

	ehttptest.Eq(t, "RFC 9110 §13.2.1", resp.StatusCode, 500)
	ehttptest.Eq(t, "RFC 9110 §13.2.1", body, "error body")
}
