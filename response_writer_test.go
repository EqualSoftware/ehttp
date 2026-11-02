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
	"sync"
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

func TestResponseWriterAppendMergesMembers(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) {
		res.Append("X-A", "1")
		res.Append("X-A", "2")
		res.End()
	})

	ehttptest.Eq(t, "author decision 2026-11-01 (strict premise / header API): Append merges list members in one line", strings.Join(rec.Header().Values("X-A"), "|"), "1, 2")
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

			ehttptest.Eq(t, "S11: Vary compares the whole field name", vary(tt.preset, tt.values...), tt.want)
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

		got := strings.Join(rec.Header().Values(HeaderVary), ", ")
		for _, name := range []string{"A", "B", "C"} {
			ehttptest.True(t, "RFC 9110 §5.3 (multiple field lines combine)", strings.Contains(got, name), "Vary %q lost %s", got, name)
		}
	})
}

// S4 decision: writing twice is allowed, the body keeps being delivered.
func TestResponseWriterWriteTwiceOverRealServer(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.Use(aRecoverAbort)
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
		res.JSON(1)
	})
	ts := aRealServer(t, s)

	t.Run("SendStatus then String delivers status and body", func(t *testing.T) {
		t.Parallel()

		resp, body := aGetBody(t, ts.URL+"/status-then-body")
		ehttptest.Eq(t, "S4 decision (works today, must keep working)", resp.StatusCode, 201)
		ehttptest.Eq(t, "S4 decision", body, "x")
	})

	t.Run("second String panics and the connection is aborted", func(t *testing.T) {
		t.Parallel()

		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/body-twice", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		ehttptest.True(t, "S6", err == nil, "the header of the first body must reach the client: %v", err)
		ehttptest.Eq(t, "S6: first status wins", resp.StatusCode, 200)

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		ehttptest.Eq(t, "S6: the first body is complete", string(body), "first")
		ehttptest.True(t, "S6", err == nil, "sized body read: %v", err)
	})

	t.Run("JSON after String panics", func(t *testing.T) {
		t.Parallel()

		resp, body := aGetBody(t, ts.URL+"/json-after-string")
		ehttptest.Eq(t, "S6: first status wins", resp.StatusCode, 200)
		ehttptest.Eq(t, "S6: the first body is the only body", body, "a")
		ehttptest.Eq(t, "S6: headers are frozen once written", resp.Header.Get(HeaderContentType), MIMETextPlainCharsetUTF8)
	})
}

// aRecoverAbort is the Recover middleware of the middleware package: after the
// header is sent, it flushes and aborts the connection.
func aRecoverAbort(next Handler) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		defer func() {
			if r := recover(); r != nil {
				res.Flush()
				panic(http.ErrAbortHandler)
			}
		}()

		next.ServeHTTP(res, req)
	}
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
		res := &ResponseWriter{w: rec, request: httptest.NewRequestWithContext(context.Background(), "GET", "/", http.NoBody)}
		iw, i := WithResponseWriter(res)

		return rec, res, iw, i
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
		res := &ResponseWriter{w: rec, request: httptest.NewRequestWithContext(ctx, "GET", "/", http.NoBody)}
		iw, _ := WithResponseWriter(res)
		iw.String("x")

		ehttptest.Eq(t, "ResponseWriter.finished", iw.Unwrap().(*ResponseWriterInterceptor).Buffer.Len(), 0)
	})
}

func TestResponseWriterHeadersSent(t *testing.T) {
	t.Parallel()

	var before, after bool

	aRes(func(res *ResponseWriter, _ *Request) {
		before = res.HeadersSent()
		res.SendStatus(201)
		after = res.HeadersSent()
	})

	ehttptest.Eq(t, "S4: false before the first write", before, false)
	ehttptest.Eq(t, "S4: true after the first write", after, true)
}

func TestResponseWriterCookie(t *testing.T) {
	t.Parallel()

	t.Run("several cookies are several field lines", func(t *testing.T) {
		t.Parallel()

		rec := aRes(func(res *ResponseWriter, _ *Request) {
			res.Cookie(&http.Cookie{Name: "a", Value: "1", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			res.Cookie(&http.Cookie{Name: "b", Value: "2"})
			res.End()
		})

		got := rec.Header().Values(HeaderSetCookie)
		ehttptest.Eq(t, "RFC 6265 §4.1 (one Set-Cookie per cookie)", len(got), 2)
		ehttptest.Eq(t, "RFC 6265 §5.2", got[0], "a=1; HttpOnly; Secure; SameSite=Lax")
		ehttptest.Eq(t, "RFC 6265 §4.1.1", got[1], "b=2")
	})

	t.Run("invalid cookie panics", func(t *testing.T) {
		t.Parallel()

		res, _ := aCountedRes(t)
		msg := aPanics(func() { res.Cookie(&http.Cookie{Name: "bad name", Value: "1"}) })

		ehttptest.True(t, "author decision 2026-11-01 (strict premise / header API)", strings.HasPrefix(msg, "ehttp: res.Cookie"), "message %q", msg)
	})

	t.Run("ClearCookie expires the cookie at Path /", func(t *testing.T) {
		t.Parallel()

		rec := aRes(func(res *ResponseWriter, _ *Request) {
			res.ClearCookie("sid")
			res.End()
		})

		ehttptest.Eq(t, "RFC 6265 §5.2.2 (Max-Age <= 0 expires now)", rec.Header().Get(HeaderSetCookie), "sid=; Path=/; Max-Age=0")
	})
}

func TestRequestCookie(t *testing.T) {
	t.Parallel()

	req := aReq("GET", "/", nil, "Cookie", "a=1; b=2")

	ehttptest.Eq(t, "RFC 6265 §5.4", req.Cookie("b"), "2")
	ehttptest.Eq(t, "2.1: absent cookie is empty", req.Cookie("c"), "")
}

type headerCounter struct {
	*httptest.ResponseRecorder
	calls int
}

func (h *headerCounter) WriteHeader(code int) {
	h.calls++
	h.ResponseRecorder.WriteHeader(code)
}

func aCountedRes(t *testing.T) (*ResponseWriter, *headerCounter) {
	t.Helper()

	hc := &headerCounter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)

	return &ResponseWriter{w: hc, request: req, mode: Development}, hc
}

func TestResponseWriterWriteHeaderOnce(t *testing.T) {
	t.Parallel()

	sends := map[string]func(res *ResponseWriter){
		"End":        func(res *ResponseWriter) { res.End() },
		"SendStatus": func(res *ResponseWriter) { res.SendStatus(201) },
		"Send":       func(res *ResponseWriter) { res.Send([]byte("x")) },
		"String":     func(res *ResponseWriter) { res.String("x") },
		"JSON":       func(res *ResponseWriter) { res.JSON(1) },
		"Error":      func(res *ResponseWriter) { res.Error(errors.New("x")) },
		"Redirect":   func(res *ResponseWriter) { res.Redirect("/y") },
	}
	bodies := map[string]bool{"Send": true, "String": true, "JSON": true, "Error": true, "Redirect": true}

	for name, send := range sends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			res, hc := aCountedRes(t)
			send(res)

			ehttptest.Eq(t, "S4: WriteHeader is called once", hc.calls, 1)
			ehttptest.True(t, "S4", res.HeadersSent(), "HeadersSent is false")

			if bodies[name] {
				msg := aPanics(func() { send(res) })
				ehttptest.True(t, "author decision 2026-11-01 (strict premise / header API)", strings.Contains(msg, "already sent a body"), "second body: message %q", msg)
			} else if name == "End" {
				send(res) // End after End is a no-op
			}

			ehttptest.Eq(t, "S4: WriteHeader is called once", hc.calls, 1)
		})
	}
}

func TestResponseWriterStatusAfterSent(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	res.String("ok")

	ehttptest.True(t, "author decision 2026-11-01 (strict premise / header API)", strings.Contains(aPanics(func() { res.SendStatus(500) }), "already sent"), "SendStatus after send must panic")
	ehttptest.True(t, "author decision 2026-11-01 (strict premise / header API)", strings.Contains(aPanics(func() { res.Status(404) }), "already sent"), "Status after send must panic")
	ehttptest.Eq(t, "S4: StatusCode reports the wire code", res.StatusCode(), 200)
	ehttptest.Eq(t, "S4: wire code", hc.Code, 200)
}

func TestResponseWriterInterceptorSent(t *testing.T) {
	t.Parallel()

	outer, hc := aCountedRes(t)
	inner, ic := WithResponseWriter(outer)
	inner.String("x")

	ehttptest.True(t, "S4: inner is committed", inner.HeadersSent(), "inner HeadersSent is false")
	ehttptest.True(t, "S4: outer is not sent yet", !outer.HeadersSent(), "outer HeadersSent is true")
	ehttptest.Eq(t, "S4: nothing on the wire yet", hc.calls, 0)

	ic.SendResponse(outer)

	ehttptest.True(t, "S4: outer is sent", outer.HeadersSent(), "outer HeadersSent is false")
	ehttptest.Eq(t, "S4: one WriteHeader", hc.calls, 1)
	ehttptest.Eq(t, "S4: body delivered", hc.Body.String(), "x")
}

func TestResponseWriterCookieNil(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	msg := aPanics(func() { res.Cookie(nil) })

	ehttptest.True(t, "author decision 2026-11-01 (strict premise / header API)", strings.HasPrefix(msg, "ehttp: res.Cookie"), "message %q", msg)
	ehttptest.Eq(t, "author decision 2026-11-01 (strict premise / header API)", len(hc.Header().Values(HeaderSetCookie)), 0)
}

func TestResponseWriterVaryArgument(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	res.Vary("")
	res.Vary("  ")
	res.Vary(" Origin ")
	res.Vary("ORIGIN")

	ehttptest.Eq(t, "RFC 9110 §12.5.5", hc.Header().Get(HeaderVary), "Origin")
}

func TestResponseWriterStatusThenStringRealServer(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/", func(res *ResponseWriter, _ *Request) {
		res.SendStatus(201)
		res.String("body")
	})
	ts := aRealServer(t, s)

	resp, body := aGetBody(t, ts.URL+"/")
	ehttptest.Eq(t, "S4", resp.StatusCode, 201)
	ehttptest.Eq(t, "S4", body, "body")
}

func TestHasDotDot(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]bool{
		"..":          true,
		"../x":        true,
		"a/../b":      true,
		"sub/..":      true,
		`sub\..\x`:    true,
		"a..b":        false,
		"..hid":       false,
		"sub/..hid/x": false,
		"a/b.txt":     false,
	} {
		ehttptest.Eq(t, "S9: "+path, hasDotDot(path), want)
	}
}

func TestResponseWriterNativeHandler(t *testing.T) {
	t.Parallel()

	var sentAfterWrite bool

	var logBuf syncBuffer

	s := NewServer()
	s.GET("/native", WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte("partial"))
	})))
	s.GET("/sent", func(res *ResponseWriter, req *Request) {
		WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
		}))(res, req)
		sentAfterWrite = res.HeadersSent()
	})
	s.GET("/flush", func(res *ResponseWriter, _ *Request) {
		_, _ = res.Write([]byte("a"))
		ehttptest.Eq(t, "ResponseController.Flush", http.NewResponseController(res).Flush(), nil)
	})
	s.GET("/hijack", func(res *ResponseWriter, _ *Request) {
		conn, rw, err := http.NewResponseController(res).Hijack()
		ehttptest.Eq(t, "ResponseController.Hijack", err, nil)
		ehttptest.True(t, "Hijack marks sent", res.HeadersSent(), "HeadersSent is false")
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		_ = rw.Flush()
		_ = conn.Close()
	})

	ts := httptest.NewUnstartedServer(s)
	ts.Config.ErrorLog = log.New(&logBuf, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)

	resp, body := aGetBody(t, ts.URL+"/native")
	ehttptest.Eq(t, "native WriteHeader", resp.StatusCode, 202)
	ehttptest.Eq(t, "native Write", body, "partial")

	_, _ = aGetBody(t, ts.URL+"/sent")
	ehttptest.True(t, "S4: native write marks sent", sentAfterWrite, "HeadersSent is false")

	_, body = aGetBody(t, ts.URL+"/flush")
	ehttptest.Eq(t, "Flush through res", body, "a")

	_, body = aGetBody(t, ts.URL+"/hijack")
	ehttptest.Eq(t, "Hijack through res", body, "ok")

	ehttptest.True(t, "S4: no superfluous WriteHeader", !strings.Contains(logBuf.String(), "superfluous"), "log: %s", logBuf.String())
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestResponseWriterInformationalIsNotFinal(t *testing.T) {
	t.Parallel()

	var afterInfo bool

	s := NewServer()
	s.GET("/", WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(103)
		res := w.(*ResponseWriter)
		afterInfo = res.HeadersSent()
		w.WriteHeader(201)
		_, _ = w.Write([]byte("ok"))
	})))
	ts := aRealServer(t, s)

	resp, body := aGetBody(t, ts.URL+"/")
	ehttptest.Eq(t, "RFC 9110 §15.2: final status", resp.StatusCode, 201)
	ehttptest.Eq(t, "body", body, "ok")
	ehttptest.True(t, "RFC 9110 §15.2: 103 is not final", !afterInfo, "HeadersSent is true after 103")
}

func TestResponseWriterReadFromAndFileServer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ehttptest.Eq(t, "setup", os.WriteFile(filepath.Join(dir, "f.txt"), []byte("0123456789"), 0o600), nil)

	var isReaderFrom bool

	s := NewServer()
	s.GET("/rf", WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, isReaderFrom = w.(io.ReaderFrom)
		_, _ = io.Copy(w, strings.NewReader("copied"))
	})))
	s.GET("/f.txt", WrapHandler(http.FileServer(http.Dir(dir))))
	ts := aRealServer(t, s)

	_, body := aGetBody(t, ts.URL+"/rf")
	ehttptest.Eq(t, "io.Copy through ReadFrom", body, "copied")
	ehttptest.True(t, "native handler sees io.ReaderFrom", isReaderFrom, "no io.ReaderFrom")

	_, body = aGetBody(t, ts.URL+"/f.txt")
	ehttptest.Eq(t, "FileServer file", body, "0123456789")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/f.txt", http.NoBody)
	ehttptest.Eq(t, "request", err, nil)
	req.Header.Set("Range", "bytes=2-4")

	resp, err := http.DefaultClient.Do(req)
	ehttptest.Eq(t, "request", err, nil)

	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	ehttptest.Eq(t, "FileServer range status", resp.StatusCode, 206)
	ehttptest.Eq(t, "FileServer range body", string(b), "234")
}

func TestResponseWriterVaryASCIIFold(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	res.Vary("s")
	res.Vary("ſ") // long s: Unicode folds it to "s"

	ehttptest.Eq(t, "RFC 9110 §5.1: ASCII tokens", hc.Header().Get(HeaderVary), "s, ſ")
}
