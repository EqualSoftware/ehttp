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

const ownBasis = "author decision: the last writer wins; Development warns on an override of a middleware header"

// aOwner is a middleware that owns X-Owned and the Vary member Owner.
func aOwner(next Handler) HandlerFunc {
	return func(res *ResponseWriter, req *Request) {
		res.Own("X-Owned", "mw")
		res.OwnVary("Owner")
		next.ServeHTTP(res, req)
	}
}

const overrideRule = "handler overrides a middleware header"

func TestOwnedHandlerOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		do   HandlerFunc
		want string // the value at the end; "" is removed
		got  string // the handler value in the warning
	}{
		{"Set", func(res *ResponseWriter, _ *Request) { res.Set("x-owned", "handler"); res.String("ok") }, "handler", "handler"},
		{"Append", func(res *ResponseWriter, _ *Request) { res.Append("X-Owned", "a", "b"); res.String("ok") }, "mw, a, b", "a, b"},
		{"Remove", func(res *ResponseWriter, _ *Request) { res.Remove("X-Owned"); res.String("ok") }, "", ""},
		{"raw Header().Set", func(res *ResponseWriter, _ *Request) { res.Header().Set("X-Owned", "raw"); res.String("ok") }, "raw", "raw"},
		{"raw Header().Del", func(res *ResponseWriter, _ *Request) { res.Header().Del("X-Owned"); res.String("ok") }, "", ""},
		{"native handler", WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Owned", "native")
			_, _ = w.Write([]byte("ok"))
		})), "native", "native"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/Development", func(t *testing.T) {
			t.Parallel()

			rec, logs, msg := aServe(Development, tt.do, aOwner)

			ehttptest.Eq(t, ownBasis+" (no panic)", msg, "")
			ehttptest.Eq(t, ownBasis+" (handler value sent)", rec.Result().Header.Get("X-Owned"), tt.want)

			all := logs.FilterField(zap.String("rule", overrideRule)).All()
			ehttptest.Eq(t, ownBasis+" (one warning)", len(all), 1)

			if len(all) != 1 {
				return
			}

			ctx := all[0].ContextMap()
			ehttptest.Eq(t, ownBasis, all[0].Level, zapcore.WarnLevel)
			ehttptest.True(t, ownBasis+" (message)", strings.HasPrefix(all[0].Message, "ehttp: ") &&
				strings.Contains(all[0].Message, " on GET /x: ") && strings.Contains(all[0].Message, ". Fix: "), "message %q", all[0].Message)
			ehttptest.Eq(t, ownBasis+" (header)", ctx["header"], any("X-Owned"))
			ehttptest.Eq(t, ownBasis+" (middleware_value)", ctx["middleware_value"], any(`"mw"`))
			ehttptest.Eq(t, ownBasis+" (handler_value)", ctx["handler_value"], any(strconv.Quote(tt.got)))
			ehttptest.Eq(t, ownBasis+" (method)", ctx["method"], any("GET"))
			ehttptest.Eq(t, ownBasis+" (route)", ctx["route"], any("/x"))
		})

		t.Run(tt.name+"/Production", func(t *testing.T) {
			t.Parallel()

			rec, logs, msg := aServe(Production, tt.do, aOwner)

			ehttptest.Eq(t, ownBasis+" (no panic)", msg, "")
			ehttptest.Eq(t, ownBasis+" (handler value sent)", rec.Result().Header.Get("X-Owned"), tt.want)
			ehttptest.Eq(t, ownBasis+" (no log)", logs.Len(), 0)
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

	for _, mode := range []Mode{Development, Production} {
		rec, logs, msg := aServe(mode, func(res *ResponseWriter, _ *Request) {
			res.Header().Set("Vary", "Accept")
			res.String("ok")
		}, aOwner)

		ehttptest.Eq(t, ownBasis, msg, "")
		ehttptest.Eq(t, "RFC 9110 §12.5.5 (member kept, handler member kept)", rec.Result().Header.Get("Vary"), "Accept, Owner")
		ehttptest.Eq(t, ownBasis+" (Vary does not log)", logs.Len(), 0)
	}
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

func TestOwnedCorsRawWrites(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Development))
	s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) {
		res.Header().Set("Vary", "Accept")
		res.Header().Set("Access-Control-Allow-Origin", "*")
		res.String("ok")
	})

	rec := aDo(s, "GET", "/x", "Origin", "https://a.example")

	ehttptest.Eq(t, "Fetch §3.2.5 (Vary: Origin stays)", rec.Header().Get("Vary"), "Accept, Origin")
	ehttptest.Eq(t, ownBasis+" (the handler ACAO is sent)", rec.Header().Get("Access-Control-Allow-Origin"), "*")
	ehttptest.Eq(t, ownBasis+" (one warning, for ACAO)", logs.Len(), 1)
}

func TestOwnedCorsHandlerSet(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Development))
	s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) { res.Set("Access-Control-Allow-Origin", "*") })

	rec := aDo(s, "GET", "/x", "Origin", "https://a.example")

	ehttptest.Eq(t, ownBasis, rec.Header().Get("Access-Control-Allow-Origin"), "*")
	ehttptest.Eq(t, ownBasis+" (one warning)", logs.FilterField(zap.String("header", "Access-Control-Allow-Origin")).Len(), 1)
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

func TestOwnedInterceptorWarnsInDevelopment(t *testing.T) {
	t.Parallel()

	buffered := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			ires, i := WithResponseWriter(res)
			next.ServeHTTP(ires, req)
			i.SendResponse(res)
		}
	}

	do := func(res *ResponseWriter, _ *Request) { res.Set("X-Owned", "h"); res.String("ok") }

	rec, logs, msg := aServe(Production, do, aOwner, buffered)
	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Owned"), "h")
	ehttptest.Eq(t, ownBasis+" (Production: no log)", logs.Len(), 0)

	rec, logs, msg = aServe(Development, do, aOwner, buffered)
	ehttptest.Eq(t, ownBasis, msg, "")
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Owned"), "h")
	ehttptest.Eq(t, ownBasis+" (Development: one warning)", logs.Len(), 1)
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

func TestOwnManyFieldsKeepsEveryOne(t *testing.T) {
	t.Parallel()

	many := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) {
			for _, n := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N"} {
				res.Own("X-"+n, n)
			}

			next.ServeHTTP(res, req)
		}
	}

	rec, logs, _ := aServe(Development, func(res *ResponseWriter, _ *Request) { res.Set("X-N", "h"); res.String("ok") }, many)

	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-N"), "h")
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-M"), "M")
	ehttptest.Eq(t, ownBasis, logs.Len(), 1)
}

func TestProductionTracksNoOwner(t *testing.T) {
	t.Parallel()

	var tracked bool

	aServe(Production, func(res *ResponseWriter, _ *Request) {
		tracked = res.dev != nil
		res.End()
	}, aOwner)

	ehttptest.True(t, ownBasis+" (no owner list)", !tracked, "tracked")
}

func TestOwnedEqualValueIsNotAMistake(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Development))
	s.Use(Cors(CorsOptions{Origin: []string{"*"}}))
	s.GET("/x", func(res *ResponseWriter, _ *Request) {
		res.Set("Access-Control-Allow-Origin", "*")
		res.String("ok")
	})

	rec := aDo(s, "GET", "/x", "Origin", "https://a.example")

	ehttptest.Eq(t, ownBasis, rec.Header().Get("Access-Control-Allow-Origin"), "*")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)

	do := func(res *ResponseWriter, _ *Request) { res.Append("X-Owned", "mw"); res.String("ok") }
	rec, logs, msg := aServe(Development, do, aOwner)
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
	rec, logs, _ := aServe(Development, do, buffered, aOwner)

	ehttptest.Eq(t, ownBasis, rec.Result().Header.Get("X-Owned"), "raw")
	ehttptest.Eq(t, ownBasis+" (one warning)", logs.Len(), 1)

	// The inner owner changes a value that the outer writer owns.
	inner := func(next Handler) HandlerFunc {
		return func(res *ResponseWriter, req *Request) { res.Own("X-Owned", "inner"); next.ServeHTTP(res, req) }
	}

	rec, logs, _ = aServe(Development, func(res *ResponseWriter, _ *Request) { res.End() }, aOwner, buffered, inner)
	ehttptest.Eq(t, ownBasis+" (the inner owner is newer)", rec.Result().Header.Get("X-Owned"), "inner")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
}

func TestOwnedFieldValuesAreQuoted(t *testing.T) {
	t.Parallel()

	_, logs, _ := aServe(Development, func(res *ResponseWriter, _ *Request) {
		res.Set("X-Owned", "h\tb")
		res.String("ok")
	}, aOwner)

	ehttptest.Eq(t, ownBasis, logs.Len(), 1)

	for _, e := range logs.All() {
		for _, f := range e.Context {
			ehttptest.True(t, ownBasis+" (no raw CR or LF in a field)", !strings.ContainsAny(f.String, "\t"), "field %s = %q", f.Key, f.String)
		}
	}
}

// TestOwnedRealServer sends the response over a real connection.
func TestOwnedRealServer(t *testing.T) {
	t.Parallel()

	for _, mode := range []Mode{Development, Production} {
		l, logs := aObserved()
		s := NewServer(WithLogger(l), WithMode(mode))
		s.Use(Cors(CorsOptions{Origin: []string{"https://a.example"}}))
		s.GET("/x", func(res *ResponseWriter, _ *Request) {
			res.Header().Del("Vary")
			res.Set("Access-Control-Allow-Origin", "https://handler.example")
			res.String("ok")
		})

		srv := httptest.NewServer(s)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/x", http.NoBody)
		ehttptest.Eq(t, ownBasis, err, nil)
		req.Header.Set("Origin", "https://a.example")

		resp, err := srv.Client().Do(req)
		ehttptest.Eq(t, ownBasis, err, nil)
		_ = resp.Body.Close()
		srv.Close()

		ehttptest.Eq(t, ownBasis+" (handler value wins)", resp.Header.Get("Access-Control-Allow-Origin"), "https://handler.example")
		ehttptest.Eq(t, "Fetch §3.2.5 (Vary: Origin survives)", resp.Header.Get("Vary"), "Origin")

		if mode == Development {
			ehttptest.Eq(t, ownBasis+" (Development warns)", logs.FilterField(zap.String("rule", overrideRule)).Len(), 1)
			continue
		}

		ehttptest.Eq(t, ownBasis+" (Production logs nothing)", logs.Len(), 0)
	}
}

func TestQuoteValueCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()

	v := strings.Repeat("a", 63) + "é" + "tail" // the rune spans bytes 63-64

	got := quoteValue(v)

	ehttptest.True(t, ownBasis, strings.HasSuffix(got, `"...`) && !strings.Contains(got, `\x`), "got %s", got)
}
