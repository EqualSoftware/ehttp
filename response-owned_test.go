package ehttp

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const ownBasis = "a middleware owns the header: the handler cannot change it"

// aOwner is a middleware that owns X-Owned and the Vary member Owner.
func aOwner(next Handler) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		res.Own("X-Owned", "mw")
		res.OwnVary("Owner")
		next.ServeHTTP(res, req)
	}
}

func TestOwnedHandlerOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		do        HandlerFunc
		attempted string
		want      string // the owned value at the end
	}{
		{"Set", func(res *ResponseWriter, _ *Request) { res.Set("x-owned", "handler"); res.String("ok") }, "handler", "mw"},
		{"Append", func(res *ResponseWriter, _ *Request) { res.Append("X-Owned", "a", "b"); res.String("ok") }, "a, b", "mw"},
		{"Remove", func(res *ResponseWriter, _ *Request) { res.Remove("X-Owned"); res.String("ok") }, "", "mw"},
		{"raw Header().Set", func(res *ResponseWriter, _ *Request) { res.Header().Set("X-Owned", "raw"); res.String("ok") }, "raw", "mw"},
		{"raw Header().Del", func(res *ResponseWriter, _ *Request) { res.Header().Del("X-Owned"); res.String("ok") }, "", "mw"},
		{"native handler", WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Owned", "native")
			_, _ = w.Write([]byte("ok"))
		})), "native", "mw"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/Development", func(t *testing.T) {
			t.Parallel()

			_, _, msg := aServe(Development, tt.do, aOwner)

			ehttptest.True(t, ownBasis, strings.HasPrefix(msg, "ehttp: "), "message %q", msg)
			ehttptest.True(t, ownBasis+" (where)", strings.Contains(msg, " on GET /x: "), "message %q", msg)
			ehttptest.True(t, ownBasis+" (why)", strings.Contains(msg, "header X-Owned is owned by a middleware, with the value \"mw\""), "message %q", msg)
			ehttptest.True(t, ownBasis+" (fix)", strings.Contains(msg, ". Fix: do not set X-Owned in the handler"), "message %q", msg)
		})

		t.Run(tt.name+"/Production", func(t *testing.T) {
			t.Parallel()

			rec, logs, msg := aServe(Production, tt.do, aOwner)

			ehttptest.Eq(t, ownBasis+" (no panic)", msg, "")
			ehttptest.Eq(t, ownBasis+" (middleware value kept)", rec.Result().Header.Get("X-Owned"), tt.want)

			all := logs.FilterField(zap.String("rule", "handler overrides a middleware header")).All()
			ehttptest.Eq(t, ownBasis+" (one warning)", len(all), 1)

			if len(all) != 1 {
				return
			}

			ctx := all[0].ContextMap()
			ehttptest.Eq(t, ownBasis, all[0].Level, zapcore.WarnLevel)
			ehttptest.Eq(t, ownBasis+" (header)", ctx["header"], any("X-Owned"))
			ehttptest.Eq(t, ownBasis+" (middleware_value)", ctx["middleware_value"], any(`"mw"`))
			ehttptest.Eq(t, ownBasis+" (attempted_value)", ctx["attempted_value"], any(strconv.Quote(tt.attempted)))
			ehttptest.Eq(t, ownBasis+" (route)", ctx["route"], any("/x"))
			ehttptest.Eq(t, ownBasis+" (one route field)", countKey(all[0], "route"), 1)
		})
	}
}

func TestOwnedHandlerCanAddVaryMembers(t *testing.T) {
	t.Parallel()

	rec, logs, msg := aServe(Development, func(res *ResponseWriter, _ *Request) {
		res.Vary("Accept")
		res.String("ok")
	}, aOwner)

	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
	ehttptest.Eq(t, ownBasis, rec.Header().Get("Vary"), "Owner, Accept")
}

func TestOwnedVaryMemberIsRestored(t *testing.T) {
	t.Parallel()

	do := func(res *ResponseWriter, _ *Request) {
		res.Header().Set("Vary", "Accept")
		res.String("ok")
	}

	rec, logs, msg := aServe(Production, do, aOwner)

	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (member restored, handler member kept)", rec.Result().Header.Get("Vary"), "Accept, Owner")
	ehttptest.Eq(t, ownBasis+" (one warning)", logs.Len(), 1)

	ctx := logs.All()[0].ContextMap()
	ehttptest.Eq(t, ownBasis, ctx["header"], any("Vary"))
	ehttptest.Eq(t, ownBasis, ctx["middleware_value"], any(`"Owner"`))
	ehttptest.Eq(t, ownBasis, ctx["attempted_value"], any(`"Accept"`))

	_, _, msg = aServe(Development, do, aOwner)
	ehttptest.True(t, ownBasis+" (Development panics)", strings.Contains(msg, "header Vary is owned by a middleware"), "message %q", msg)
}

func TestOwnedVaryWildcardKeepsMembers(t *testing.T) {
	t.Parallel()

	rec, logs, msg := aServe(Development, func(res *ResponseWriter, _ *Request) {
		res.Vary("*")
		res.String("ok")
	}, aOwner)

	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (* covers every member)", logs.Len(), 0)
	ehttptest.Eq(t, ownBasis, rec.Header().Get("Vary"), "*")
}

func TestOwnedCorsRawVary(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithCors(CorsOptions{Origin: []string{"https://a.example"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) {
		res.Header().Set("Vary", "Accept")
		res.Header().Set("Access-Control-Allow-Origin", "*")
		res.String("ok")
	})

	rec := aDo(s, "GET", "/x", "Origin", "https://a.example")

	ehttptest.Eq(t, "Fetch §3.2.5 (Vary: Origin is restored)", rec.Header().Get("Vary"), "Accept, Origin")
	ehttptest.Eq(t, ownBasis+" (ACAO is restored)", rec.Header().Get("Access-Control-Allow-Origin"), "https://a.example")
	ehttptest.Eq(t, ownBasis+" (one warning for each field)", logs.Len(), 2)
}

func TestOwnedCorsHandlerSet(t *testing.T) {
	t.Parallel()

	s := NewServer(WithMode(Development), WithCors(CorsOptions{Origin: []string{"https://a.example"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) { res.Set("Access-Control-Allow-Origin", "*") })

	msg := aPanics(func() { aDo(s, "GET", "/x", "Origin", "https://a.example") })

	ehttptest.True(t, ownBasis, strings.Contains(msg, "header Access-Control-Allow-Origin is owned by a middleware"), "message %q", msg)
}

func TestOwnedMiddlewareReplacesMiddleware(t *testing.T) {
	t.Parallel()

	second := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			res.Own("X-Owned", "second")
			next.ServeHTTP(res, req)
		}
	}

	rec, logs, msg := aServe(Development, func(res *ResponseWriter, _ *Request) { res.String("ok") }, aOwner, second)

	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
	ehttptest.Eq(t, ownBasis+" (the last owner wins)", rec.Header().Get("X-Owned"), "second")
}

func TestOwnedAfterTheHandlerReplacesToo(t *testing.T) {
	t.Parallel()

	after := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			ires, i := WithResponseWriter(res)
			next.ServeHTTP(ires, req)
			res.Own("X-Owned", "after")
			i.SendResponse(res)
		}
	}

	rec, logs, msg := aServe(Development, func(res *ResponseWriter, _ *Request) { res.String("ok") }, aOwner, after)

	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Owned"), "after")
}

func TestOwnedInterceptorKeepsOwnership(t *testing.T) {
	t.Parallel()

	buffered := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			ires, i := WithResponseWriter(res)
			next.ServeHTTP(ires, req)
			i.SendResponse(res)
		}
	}

	rec, logs, _ := aServe(Production, func(res *ResponseWriter, _ *Request) { res.Set("X-Owned", "h"); res.String("ok") }, aOwner, buffered)

	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Owned"), "mw")
	ehttptest.Eq(t, ownBasis, logs.Len(), 1)

	_, _, msg := aServe(Development, func(res *ResponseWriter, _ *Request) { res.Set("X-Owned", "h") }, aOwner, buffered)
	ehttptest.True(t, ownBasis+" (the inner writer panics too)", strings.Contains(msg, "is owned by a middleware"), "message %q", msg)
}

func TestOwnRejectsSpecialFields(t *testing.T) {
	t.Parallel()

	_, _, msg := aServe(Development, func(res *ResponseWriter, _ *Request) { res.Own("Vary", "a") })
	ehttptest.True(t, ownBasis, strings.Contains(msg, "Fix: use res.OwnVary("), "message %q", msg)

	_, _, msg = aServe(Development, func(res *ResponseWriter, _ *Request) { res.Own("Set-Cookie", "a=1") })
	ehttptest.True(t, ownBasis, strings.Contains(msg, "Fix: use res.Cookie("), "message %q", msg)

	_, _, msg = aServe(Development, func(res *ResponseWriter, _ *Request) { res.Own("Bad Name", "a") })
	ehttptest.True(t, ownBasis, strings.HasPrefix(msg, `ehttp: res.Own("Bad Name", ...) on GET /x: `), "message %q", msg)
}

func TestOwnManyFieldsSpillsWithoutLosingOne(t *testing.T) {
	t.Parallel()

	many := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			for _, n := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N"} {
				res.Own("X-"+n, n)
			}

			next.ServeHTTP(res, req)
		}
	}

	rec, logs, _ := aServe(Production, func(res *ResponseWriter, _ *Request) { res.Set("X-N", "h"); res.String("ok") }, many)

	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-N"), "N")
	ehttptest.Eq(t, ownBasis, logs.Len(), 1)
}

func TestOwnedFieldsDoNotAllocatePerRequest(t *testing.T) {
	l, _ := aObserved()
	names := []string{"X-A", "X-B"} // with the Vary member: the 3 inline slots

	raw := NewServer(WithLogger(l))
	raw.GET("/x", func(res *ResponseWriter, _ *Request) { res.End() }, func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			for _, n := range names {
				res.Header().Set(n, "v")
			}

			res.Header().Set("Vary", "Origin")
			next.ServeHTTP(res, req)
		}
	})

	owning := NewServer(WithLogger(l))
	owning.GET("/x", func(res *ResponseWriter, _ *Request) { res.End() }, func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			for _, n := range names {
				res.Own(n, "v")
			}

			res.OwnVary("Origin")
			next.ServeHTTP(res, req)
		}
	})

	r := httptest.NewRequestWithContext(t.Context(), "GET", "/x", http.NoBody)
	w := httptest.NewRecorder()

	base := testing.AllocsPerRun(100, func() { raw.ServeHTTP(w, r) })
	used := testing.AllocsPerRun(100, func() { owning.ServeHTTP(w, r) })

	ehttptest.True(t, ownBasis+" (the first owned fields are inline)", used <= base, "raw %v, owned %v", base, used)
}

func TestOwnedEqualValueIsNotAMistake(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Development), WithCors(CorsOptions{Origin: []string{"*"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) {
		res.Set("Access-Control-Allow-Origin", "*")
		res.String("ok")
	})

	rec := aDo(s, "GET", "/x", "Origin", "https://a.example")

	ehttptest.Eq(t, ownBasis, rec.Header().Get("Access-Control-Allow-Origin"), "*")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)

	do := func(res *ResponseWriter, _ *Request) { res.Append("X-Owned", "mw"); res.String("ok") }
	rec, logs, msg := aServe(Production, do, aOwner)
	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis+" (Append of a member that is there)", logs.Len(), 0)
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Owned"), "mw")
}

func TestOwnedBufferedWriterOutsideOwner(t *testing.T) {
	t.Parallel()

	buffered := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			ires, i := WithResponseWriter(res)
			next.ServeHTTP(ires, req)
			i.SendResponse(res)
		}
	}

	// The handler writes no body; the owner is inside the buffered writer.
	do := func(res *ResponseWriter, _ *Request) { res.Header().Set("X-Owned", "raw") }
	rec, logs, _ := aServe(Production, do, buffered, aOwner)

	ehttptest.Eq(t, ownBasis, rec.Result().Header.Get("X-Owned"), "mw")
	ehttptest.Eq(t, ownBasis+" (one warning)", logs.Len(), 1)

	// The inner owner changes a value that the outer writer owns.
	inner := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) { res.Own("X-Owned", "inner"); next.ServeHTTP(res, req) }
	}

	rec, logs, _ = aServe(Production, func(res *ResponseWriter, _ *Request) { res.End() }, aOwner, buffered, inner)
	ehttptest.Eq(t, ownBasis+" (the inner owner is newer)", rec.Result().Header.Get("X-Owned"), "inner")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
}

func TestOwnedFieldValuesAreQuoted(t *testing.T) {
	t.Parallel()

	_, logs, _ := aServe(Production, func(res *ResponseWriter, _ *Request) {
		res.Set("X-A", "a\r\nInjected: 1")
		res.Set("X-Owned", "h\r\n")
		res.String("ok")
	}, aOwner)

	for _, e := range logs.All() {
		for _, f := range e.Context {
			ehttptest.True(t, ownBasis+" (no raw CR or LF in a field)", !strings.ContainsAny(f.String, "\r\n"), "field %s = %q", f.Key, f.String)
		}
	}
}

func TestQuoteValueCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()

	v := strings.Repeat("a", 63) + "é" + "tail" // the rune spans bytes 63-64

	got := quoteValue(v)

	ehttptest.True(t, ownBasis, strings.HasSuffix(got, `"...`) && !strings.Contains(got, `\x`), "got %s", got)
}
