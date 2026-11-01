package ehttp

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aRes serves GET / through a server whose only route is h.
func aRes(h HandlerFunc, kv ...string) *httptest.ResponseRecorder {
	s := NewServer()
	s.GET("/", h)

	return aDo(s, "GET", "/", kv...)
}

// aRealServer starts a real net/http server (lifecycle rule of lessons.md)
// with the superfluous-WriteHeader log silenced.
func aRealServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	ts := httptest.NewUnstartedServer(h)
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)

	return ts
}

func aGetBody(t *testing.T, url string) (resp *http.Response, body string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)

	return resp, string(b)
}

func TestResponseWriterSenders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		basis string
		h     HandlerFunc
		code  int
		ctype string
		body  string
		loc   string
	}{
		{"String", "Express res.send(string)", func(res *ResponseWriter, _ *Request) { res.String("hi") }, 200, MIMETextPlainCharsetUTF8, "hi", ""},
		{"String after Status", "Express res.status().send()", func(res *ResponseWriter, _ *Request) { res.Status(201); res.String("hi") }, 201, MIMETextPlainCharsetUTF8, "hi", ""},
		{"JSON", "Express res.json", func(res *ResponseWriter, _ *Request) { res.JSON(map[string]int{"a": 1}) }, 200, MIMEApplicationJSONCharsetUTF8, `{"a":1}`, ""},
		{"JSON after Status", "Express res.status().json()", func(res *ResponseWriter, _ *Request) { res.Status(422); res.JSON([]int{1}) }, 422, MIMEApplicationJSONCharsetUTF8, `[1]`, ""},
		{"Send bytes", "Express res.send(Buffer)", func(res *ResponseWriter, _ *Request) { res.Send([]byte("raw")) }, 200, "", "raw", ""},
		{"Error defaults to 500", "RFC 9110 §15.6.1", func(res *ResponseWriter, _ *Request) { res.Error(errors.New("boom")) }, 500, MIMETextPlainCharsetUTF8, "boom", ""},
		{"Error keeps Status", "Express res.status(400)", func(res *ResponseWriter, _ *Request) { res.Status(400); res.Error(errors.New("bad")) }, 400, MIMETextPlainCharsetUTF8, "bad", ""},
		{"SendStatus has no body", "characterization: Express res.sendStatus would send the status text", func(res *ResponseWriter, _ *Request) { res.SendStatus(404) }, 404, "", "", ""},
		{"SendStatus 204", "RFC 9110 §15.3.5", func(res *ResponseWriter, _ *Request) { res.SendStatus(204) }, 204, "", "", ""},
		{"End defaults to 200", "Express res.end()", func(res *ResponseWriter, _ *Request) { res.End() }, 200, "", "", ""},
		{"End keeps Status", "Express res.status().end()", func(res *ResponseWriter, _ *Request) { res.Status(202); res.End() }, 202, "", "", ""},
		{"Redirect defaults to 307", "RFC 9110 §15.4.8", func(res *ResponseWriter, _ *Request) { res.Redirect("/new") }, 307, "", "", "/new"},
		{"Redirect keeps Status", "RFC 9110 §15.4.2", func(res *ResponseWriter, _ *Request) { res.Status(301); res.Redirect("/new") }, 301, "", "", "/new"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := aRes(tt.h)
			ehttptest.Eq(t, tt.basis, rec.Code, tt.code)
			ehttptest.Eq(t, tt.basis+" (Content-Type)", rec.Header().Get(HeaderContentType), tt.ctype)
			ehttptest.Eq(t, tt.basis+" (body)", rec.Body.String(), tt.body)
			ehttptest.Eq(t, tt.basis+" (Location)", rec.Header().Get(HeaderLocation), tt.loc)

			if tt.body != "" {
				ehttptest.Eq(t, "RFC 9110 §8.6", rec.Header().Get(HeaderContentLength), strconv.Itoa(len(tt.body)))
			}
		})
	}
}

func TestResponseWriterContentLengthCountsBytes(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) { res.String("é€") })
	ehttptest.Eq(t, "RFC 9110 §8.6 (octets, not characters)", rec.Header().Get(HeaderContentLength), "5")
}

func TestResponseWriterRedirectClearsBodyHeaders(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) {
		res.Header().Set(HeaderContentType, "text/plain")
		res.Header().Set(HeaderContentLength, "10")
		res.Redirect("/x")
	})

	ehttptest.Eq(t, "RFC 9110 §15.4 (redirect has no representation)", rec.Header().Get(HeaderContentType), "")
	ehttptest.Eq(t, "RFC 9110 §15.4", rec.Header().Get(HeaderContentLength), "")
}

func TestResponseWriterStatusAndLocationDoNotSend(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) {
		res.Status(201)
		res.Location("/loc")
	})

	ehttptest.Eq(t, "Express res.status() only sets the code", rec.Body.Len(), 0)
	ehttptest.Eq(t, "Express res.location()", rec.Header().Get(HeaderLocation), "/loc")
}

func TestResponseWriterStatusCode(t *testing.T) {
	t.Parallel()

	var before, afterStatus, afterSend int

	aRes(func(res *ResponseWriter, _ *Request) {
		before = res.StatusCode()
		res.Status(202)
		afterStatus = res.StatusCode()
		res.String("x")
		afterSend = res.StatusCode()
	})

	ehttptest.Eq(t, "Status sets the code", afterStatus, 202)
	ehttptest.Eq(t, "String keeps the code", afterSend, 202)

	t.Run("default before anything is written", func(t *testing.T) {
		t.Parallel()

		ehttptest.KnownBug(t, "NA-5", func(t testing.TB) {
			ehttptest.Eq(t, "Node http.ServerResponse.statusCode defaults to 200", before, 200)
		})
	})
}

func TestResponseWriterAppendReplaces(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) {
		res.Append("X-A", "1")
		res.Append("X-A", "2")
		res.End()
	})

	ehttptest.Eq(t, "2.1: Append has Header().Set semantics", strings.Join(rec.Header().Values("X-A"), ","), "2")
}

func TestResponseWriterVary(t *testing.T) {
	t.Parallel()

	vary := func(preset string, values ...string) string {
		rec := aRes(func(res *ResponseWriter, _ *Request) {
			if preset != "" {
				res.Header().Set(HeaderVary, preset)
			}

			for _, v := range values {
				res.Vary(v)
			}

			res.End()
		})

		return rec.Header().Get(HeaderVary)
	}

	tests := []struct {
		name, basis, preset string
		values              []string
		want                string
	}{
		{"first value", "RFC 9110 §12.5.5", "", []string{"Origin"}, "Origin"},
		{"two values", "RFC 9110 §12.5.5", "", []string{"Origin", "Accept"}, "Origin, Accept"},
		{"three values", "RFC 9110 §12.5.5", "", []string{"A", "B", "C"}, "A, B, C"},
		{"same name twice", "RFC 9110 §12.5.5 (no duplicates)", "", []string{"Origin", "Origin"}, "Origin"},
		{"name is case-insensitive", "RFC 9110 §5.1", "", []string{"Origin", "origin"}, "Origin"},
		{"preset without spaces is normalized", "RFC 9110 §5.6.1", "A,B", []string{"C"}, "A, B, C"},
		{"preset already has the value", "RFC 9110 §12.5.5", "A, B", []string{"B"}, "A, B"},
		{"longer name added after its own prefix", "RFC 9110 §12.5.5", "Accept", []string{"Accept-Encoding"}, "Accept, Accept-Encoding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ehttptest.Eq(t, tt.basis, vary(tt.preset, tt.values...), tt.want)
		})
	}

	// S11: substring comparison drops a name that is a prefix of an existing one.
	for _, tt := range []struct {
		name, preset string
		values       []string
		want         string
	}{
		{"Accept after Accept-Encoding", "", []string{"Accept-Encoding", "Accept"}, "Accept-Encoding, Accept"},
		{"Accept after Accept-Language (preset)", "Accept-Language", []string{"Accept"}, "Accept-Language, Accept"},
		{"Origin after X-Origin", "", []string{"X-Origin", "Origin"}, "X-Origin, Origin"},
	} {
		t.Run("S11 "+tt.name, func(t *testing.T) {
			t.Parallel()

			ehttptest.KnownBug(t, "S11", func(t testing.TB) {
				ehttptest.Eq(t, "S11: Vary compares the whole field name", vary(tt.preset, tt.values...), tt.want)
			})
		})
	}

	t.Run("existing Vary field lines are all kept", func(t *testing.T) {
		t.Parallel()

		rec := aRes(func(res *ResponseWriter, _ *Request) {
			res.Header().Add(HeaderVary, "A")
			res.Header().Add(HeaderVary, "B")
			res.Vary("C")
			res.End()
		})

		ehttptest.KnownBug(t, "NA-3", func(t testing.TB) {
			got := strings.Join(rec.Header().Values(HeaderVary), ", ")
			for _, name := range []string{"A", "B", "C"} {
				ehttptest.True(t, "RFC 9110 §5.3 (multiple field lines combine)", strings.Contains(got, name), "Vary %q lost %s", got, name)
			}
		})
	})
}

// S4 decision: writing twice is allowed, the body keeps being delivered.
func TestResponseWriterWriteTwiceOverRealServer(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/status-then-body", func(res *ResponseWriter, _ *Request) {
		res.SendStatus(201)
		res.String("x")
	})
	s.GET("/body-twice", func(res *ResponseWriter, _ *Request) {
		res.String("first")
		res.String("second")
	})
	s.GET("/json-after-string", func(res *ResponseWriter, _ *Request) {
		res.String("a")
		res.Status(500)
		res.JSON(1)
	})
	ts := aRealServer(t, s)

	t.Run("SendStatus then String delivers status and body", func(t *testing.T) {
		t.Parallel()

		resp, body := aGetBody(t, ts.URL+"/status-then-body")
		ehttptest.Eq(t, "S4 decision (works today, must keep working)", resp.StatusCode, 201)
		ehttptest.Eq(t, "S4 decision", body, "x")
	})

	t.Run("second String does not corrupt the first response", func(t *testing.T) {
		t.Parallel()

		resp, body := aGetBody(t, ts.URL+"/body-twice")
		ehttptest.Eq(t, "S4: first status wins", resp.StatusCode, 200)
		ehttptest.Eq(t, "S4: net/http enforces the first Content-Length", body, "first")
	})

	t.Run("later status and headers are ignored", func(t *testing.T) {
		t.Parallel()

		resp, _ := aGetBody(t, ts.URL+"/json-after-string")
		ehttptest.Eq(t, "S4: first status wins", resp.StatusCode, 200)
		ehttptest.Eq(t, "S4: headers are frozen once written", resp.Header.Get(HeaderContentType), MIMETextPlainCharsetUTF8)
	})
}

func TestResponseWriterFinishedRequestIsInert(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()

	senders := map[string]HandlerFunc{
		"String":     func(res *ResponseWriter, _ *Request) { res.String("x") },
		"JSON":       func(res *ResponseWriter, _ *Request) { res.JSON(1) },
		"Send":       func(res *ResponseWriter, _ *Request) { res.Send([]byte("x")) },
		"Error":      func(res *ResponseWriter, _ *Request) { res.Error(errors.New("x")) },
		"SendStatus": func(res *ResponseWriter, _ *Request) { res.SendStatus(500) },
		"End":        func(res *ResponseWriter, _ *Request) { res.Status(500); res.End() },
		"Redirect":   func(res *ResponseWriter, _ *Request) { res.Redirect("/x") },
		"Render":     func(res *ResponseWriter, _ *Request) { res.Render("none", nil) },
		"Header":     func(res *ResponseWriter, _ *Request) { res.Header().Set("X-Late", "1") },
		"Append":     func(res *ResponseWriter, _ *Request) { res.Append("X-Late", "1") },
		"Vary":       func(res *ResponseWriter, _ *Request) { res.Vary("Origin") },
	}

	for ctxName, ctx := range map[string]context.Context{"canceled": canceled, "deadline exceeded": expired} {
		for name, h := range senders {
			t.Run(ctxName+"/"+name, func(t *testing.T) {
				t.Parallel()

				s := NewServer()
				s.GET("/", h)

				req := httptest.NewRequestWithContext(ctx, "GET", "/", http.NoBody)
				spy := &aSpyWriter{header: http.Header{}}
				s.ServeHTTP(spy, req)

				// The recorder code is 200 by default, so count the calls instead.
				basis := "ResponseWriter.finished (design 2.3: late writes are dropped)"
				ehttptest.Eq(t, basis, spy.headerCalls, 0)
				ehttptest.Eq(t, basis, spy.bodyBytes, 0)
				ehttptest.Eq(t, basis, spy.header.Get("X-Late"), "")
				ehttptest.Eq(t, basis, spy.header.Get(HeaderContentType), "")
			})
		}
	}
}

func aTemplateServer(t *testing.T, files map[string]string, opts ...serverOption) *Server {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return NewServer(append([]serverOption{WithTemplates(filepath.Join(dir, "*.html"))}, opts...)...)
}

// aSpyWriter counts what reaches the wire (WriteHeader and Write calls).
type aSpyWriter struct {
	header      http.Header
	headerCalls int
	bodyBytes   int
}

func (w *aSpyWriter) Header() http.Header { return w.header }
func (w *aSpyWriter) WriteHeader(int)     { w.headerCalls++ }

func (w *aSpyWriter) Write(b []byte) (int, error) {
	w.bodyBytes += len(b)
	return len(b), nil
}

func TestResponseWriterRender(t *testing.T) {
	t.Parallel()

	s := aTemplateServer(t, map[string]string{"page.html": `<p>{{.}}</p>`})
	s.GET("/ok", func(res *ResponseWriter, _ *Request) { res.Render("page.html", "<b>hi</b>") })
	s.GET("/created", func(res *ResponseWriter, _ *Request) { res.Status(201); res.Render("page.html", "x") })
	h := s

	t.Run("renders with html headers", func(t *testing.T) {
		t.Parallel()

		rec := aDo(h, "GET", "/ok")
		ehttptest.Eq(t, "Express res.render", rec.Code, 200)
		ehttptest.Eq(t, "Express res.render", rec.Header().Get(HeaderContentType), MIMETextHTMLCharsetUTF8)
		ehttptest.Eq(t, "html/template contextual escaping (OWASP XSS)", rec.Body.String(), `<p>&lt;b&gt;hi&lt;/b&gt;</p>`)
		ehttptest.Eq(t, "RFC 9110 §8.6", rec.Header().Get(HeaderContentLength), strconv.Itoa(rec.Body.Len()))
	})

	t.Run("keeps Status", func(t *testing.T) {
		t.Parallel()
		ehttptest.Eq(t, "Express res.status().render()", aDo(h, "GET", "/created").Code, 201)
	})
}

func TestResponseWriterPanicsWithoutWritingAnything(t *testing.T) {
	t.Parallel()

	s := aTemplateServer(t, map[string]string{"page.html": `x`})
	s.GET("/missing-template", func(res *ResponseWriter, _ *Request) { res.Render("nope.html", nil) })
	s.GET("/bad-json", func(res *ResponseWriter, _ *Request) { res.JSON(make(chan int)) })

	bare := NewServer()
	bare.GET("/no-engine", func(res *ResponseWriter, _ *Request) { res.Render("page.html", nil) })

	tests := []struct {
		name   string
		h      http.Handler
		target string
		basis  string
	}{
		{"unknown template", s, "/missing-template", "Express res.render throws on a missing view"},
		{"unserializable JSON", s, "/bad-json", "Express res.json throws on unserializable value"},
		{"no templates configured", bare, "/no-engine", "Express res.render throws without a view engine"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				tt.h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", tt.target, http.NoBody))

				return false
			}()

			ehttptest.True(t, tt.basis, panicked, "expected a panic")
			ehttptest.Eq(t, "nothing is sent before the panic", rec.Body.Len(), 0)
			ehttptest.Eq(t, "nothing is sent before the panic", rec.Header().Get(HeaderContentType), "")
		})
	}
}

func TestResponseWriterInterceptor(t *testing.T) {
	t.Parallel()

	newPair := func() (*httptest.ResponseRecorder, *ResponseWriter, *ResponseWriter, *ResponseWriterInterceptor) {
		rec := httptest.NewRecorder()
		res := &ResponseWriter{Writer: rec, request: httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)}
		iw := WithResponseWriter(res)

		return rec, res, iw, iw.Writer.(*ResponseWriterInterceptor)
	}

	t.Run("buffers instead of writing", func(t *testing.T) {
		t.Parallel()

		rec, _, iw, ic := newPair()
		iw.Status(201)
		iw.String("hi")

		ehttptest.Eq(t, "WithResponseWriter contract", rec.Body.Len(), 0)
		ehttptest.Eq(t, "WithResponseWriter contract", ic.Buffer.String(), "hi")
		ehttptest.Eq(t, "WithResponseWriter contract", ic.Code, 201)
		ehttptest.Eq(t, "WithResponseWriter contract", ic.Size, int64(2))
	})

	t.Run("headers are shared with the real writer", func(t *testing.T) {
		t.Parallel()

		rec, _, iw, _ := newPair()
		iw.String("hi")
		ehttptest.Eq(t, "WeakEtag/ErrorCache need to edit headers before SendResponse", rec.Header().Get(HeaderContentType), MIMETextPlainCharsetUTF8)
	})

	t.Run("SendResponse flushes status and body and updates the outer code", func(t *testing.T) {
		t.Parallel()

		rec, res, iw, ic := newPair()
		iw.Status(202)
		iw.String("hi")
		ic.SendResponse(res)

		ehttptest.Eq(t, "WithResponseWriter contract", rec.Code, 202)
		ehttptest.Eq(t, "WithResponseWriter contract", rec.Body.String(), "hi")
		ehttptest.Eq(t, "WithResponseWriter contract", res.StatusCode(), 202)
	})

	t.Run("SendResponse defaults to 200", func(t *testing.T) {
		t.Parallel()

		rec, res, _, ic := newPair()
		_, _ = ic.Write([]byte("x"))
		ic.SendResponse(res)
		ehttptest.Eq(t, "RFC 9110 §15.3.1", rec.Code, 200)
	})

	t.Run("SendResponse is idempotent and seals the interceptor", func(t *testing.T) {
		t.Parallel()

		rec, res, iw, ic := newPair()
		iw.String("hi")
		ic.SendResponse(res)
		ic.SendResponse(res)

		_, err := ic.Write([]byte("late"))
		ic.WriteHeader(500)

		ehttptest.Eq(t, "interceptor finalized", rec.Body.String(), "hi")
		ehttptest.True(t, "interceptor finalized", err != nil, "Write after SendResponse returned no error")
		ehttptest.Eq(t, "interceptor finalized", ic.Code, 200)
	})

	t.Run("finished request is still inert through the wrapper", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		rec := httptest.NewRecorder()
		res := &ResponseWriter{Writer: rec, request: httptest.NewRequestWithContext(ctx, "GET", "/", http.NoBody)}
		iw := WithResponseWriter(res)
		iw.String("x")

		ehttptest.Eq(t, "ResponseWriter.finished", iw.Writer.(*ResponseWriterInterceptor).Buffer.Len(), 0)
	})
}
