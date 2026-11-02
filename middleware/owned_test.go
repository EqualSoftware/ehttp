package middleware_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const ownBasis = "author decision: the last writer wins; Development warns on an override of a middleware header"

func bObserved() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)

	return zap.New(core), logs
}

// bDevPanic serves GET /x in Development mode and returns the panic message.
func bDevPanic(h ehttp.HandlerFunc, mws ...ehttp.MiddlewareHandlerFunc) (msg string) {
	s := ehttp.NewServer(ehttp.WithMode(ehttp.Development))
	s.GET("/x", h, mws...)

	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
		}
	}()

	ehttptest.Do(s, fixtures.Req("GET", "/x"))

	return ""
}

func TestOwnerMiddlewareHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mw     ehttp.MiddlewareHandlerFunc
		header string
		set    string // the value the handler tries
	}{
		{"Cache", middleware.Cache(time.Minute), "Cache-Control", "no-store"},
		{"Secure X-Frame-Options", middleware.Secure(middleware.SecureOptions{}), "X-Frame-Options", "ALLOWALL"},
		{"Secure X-Content-Type-Options", middleware.Secure(middleware.SecureOptions{}), "X-Content-Type-Options", "x"},
		{"Secure Referrer-Policy", middleware.Secure(middleware.SecureOptions{ReferrerPolicy: "no-referrer"}), "Referrer-Policy", "unsafe-url"},
		{"Secure CSP", middleware.Secure(middleware.SecureOptions{ContentSecurityPolicy: "default-src 'self'"}), "Content-Security-Policy", "default-src *"},
		{"RequestId", middleware.RequestId(), "X-Request-Id", "forged"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.Set(tt.header, tt.set); res.String("ok") }

			for _, mode := range []ehttp.Mode{ehttp.Development, ehttp.Production} {
				l, logs := bObserved()
				s := ehttp.NewServer(ehttp.WithLogger(l), ehttp.WithMode(mode))
				s.GET("/x", handler, tt.mw)
				rec := ehttptest.Do(s, fixtures.Req("GET", "/x"))

				ehttptest.Eq(t, ownBasis+" (handler value sent)", rec.Header().Get(tt.header), tt.set)

				want := 0
				if mode == ehttp.Development {
					want = 1
				}

				ehttptest.Eq(t, ownBasis+" (warnings)", logs.FilterField(zap.String("header", tt.header)).Len(), want)
			}
		})
	}
}

func TestOwnerMiddlewareNativeHandler(t *testing.T) {
	t.Parallel()

	native := ehttp.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		_, _ = w.Write([]byte("ok"))
	}))

	l, logs := bObserved()
	rec := ehttptest.Do(fixtures.HandlerLogged(l, native, middleware.Secure(middleware.SecureOptions{})), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ownBasis+" (the native value is sent)", rec.Result().Header.Get("X-Frame-Options"), "ALLOWALL")
	ehttptest.Eq(t, ownBasis+" (Production: no log)", logs.Len(), 0)

	l, logs = bObserved()
	s := ehttp.NewServer(ehttp.WithLogger(l), ehttp.WithMode(ehttp.Development))
	s.GET("/x", native, middleware.Secure(middleware.SecureOptions{}))
	rec = ehttptest.Do(s, fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ownBasis+" (Development: the native value is sent)", rec.Result().Header.Get("X-Frame-Options"), "ALLOWALL")
	ehttptest.Eq(t, ownBasis+" (Development: one warning)", logs.Len(), 1)
}

func TestOwnerMiddlewareVary(t *testing.T) {
	t.Parallel()

	handler := func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Vary("Accept")
		res.String("ok")
	}

	l, logs := bObserved()
	rec := ehttptest.Do(fixtures.HandlerLogged(l, handler, middleware.AcceptEncoding()), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ownBasis+" (the handler adds a member)", rec.Header().Get("Vary"), "Accept-Encoding, Accept")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)

	raw := func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Header().Set("Vary", "Accept")
		res.String("ok")
	}

	l, logs = bObserved()
	rec = ehttptest.Do(fixtures.HandlerLogged(l, raw, middleware.AcceptEncoding()), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "RFC 9110 §12.5.5 (the member is restored)", rec.Header().Get("Vary"), "Accept, Accept-Encoding")
	ehttptest.Eq(t, ownBasis+" (Vary does not log)", logs.Len(), 0)
}

func TestErrorCacheReplacesCacheWithoutWarning(t *testing.T) {
	t.Parallel()

	l, logs := bObserved()
	h := fixtures.HandlerLogged(l, fixtures.Status(404, "gone"), middleware.Cache(time.Minute), middleware.ErrorCache(time.Hour))
	rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

	ehttptest.True(t, ownBasis+" (a middleware can replace a middleware value)", strings.Contains(rec.Header().Get("Cache-Control"), "max-age=3600"), "Cache-Control %q", rec.Header().Get("Cache-Control"))
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)

	msg := bDevPanic(fixtures.Status(404, "gone"), middleware.Cache(time.Minute), middleware.ErrorCache(time.Hour))
	ehttptest.Eq(t, ownBasis+" (no panic in Development)", msg, "")
}

func TestErrorCacheIsTheLastWriter(t *testing.T) {
	t.Parallel()

	handler := func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		res.Status(404)
		res.Set("Cache-Control", "no-store")
		res.String("gone")
	}

	l, logs := bObserved()
	s := ehttp.NewServer(ehttp.WithLogger(l), ehttp.WithMode(ehttp.Development))
	s.GET("/x", handler, middleware.Cache(time.Minute), middleware.ErrorCache(time.Hour))
	rec := ehttptest.Do(s, fixtures.Req("GET", "/x"))

	ehttptest.True(t, ownBasis+" (ErrorCache writes after the handler)", strings.Contains(rec.Header().Get("Cache-Control"), "max-age=3600"), "Cache-Control %q", rec.Header().Get("Cache-Control"))
	ehttptest.Eq(t, ownBasis+" (one warning)", logs.FilterField(zap.String("header", "Cache-Control")).Len(), 1)
}

func TestTimeoutKeepsCacheValue(t *testing.T) {
	t.Parallel()

	slow := func(res *ehttp.ResponseWriter, req *ehttp.Request) { <-req.Context().Done() }

	l, logs := bObserved()
	rec := ehttptest.Do(fixtures.HandlerLogged(l, slow, middleware.Cache(time.Minute), middleware.Timeout(time.Millisecond)), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, "RFC 9110 §15.6.4", rec.Code, 503)
	ehttptest.Eq(t, "RFC 9110 §15.6.4 (Timeout sets no Cache-Control)", rec.Header().Get("Cache-Control"), ehttp.CacheControl(time.Minute, false))
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
}

func TestSetCacheHeaderIsNotAnOwner(t *testing.T) {
	t.Parallel()

	handler := func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		ehttp.SetCacheHeader(res, time.Hour)
		res.String("ok")
	}

	// Under Cache the handler value is sent in Production, with no log.
	l, logs := bObserved()
	rec := ehttptest.Do(fixtures.HandlerLogged(l, handler, middleware.Cache(time.Minute)), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ownBasis, rec.Header().Get("Cache-Control"), ehttp.CacheControl(time.Hour, false))
	ehttptest.Eq(t, ownBasis+" (no log)", logs.Len(), 0)

	msg := bDevPanic(handler, middleware.Cache(time.Minute))
	ehttptest.Eq(t, ownBasis+" (Development: no panic)", msg, "")

	// Without Cache the handler can call SetCacheHeader and then Set.
	l, logs = bObserved()
	rec = ehttptest.Do(fixtures.HandlerLogged(l, func(res *ehttp.ResponseWriter, _ *ehttp.Request) {
		ehttp.SetCacheHeader(res, time.Hour)
		res.Set("Cache-Control", "no-store")
		res.String("ok")
	}), fixtures.Req("GET", "/x"))

	ehttptest.Eq(t, ownBasis, rec.Header().Get("Cache-Control"), "no-store")
	ehttptest.Eq(t, ownBasis+" (no warning)", logs.Len(), 0)
}

func TestCacheControlIsPure(t *testing.T) {
	t.Parallel()

	ehttptest.Eq(t, "RFC 9111 §5.2.2.5", ehttp.CacheControl(0, true), "no-store, no-cache, must-revalidate, proxy-revalidate")
	ehttptest.Eq(t, "RFC 9111 §3.5", ehttp.CacheControl(time.Minute, true), "private, max-age=60, stale-while-revalidate=60, stale-if-error=60")
}

func TestNestedSecureKeepsOuterValues(t *testing.T) {
	t.Parallel()

	ok := func(res *ehttp.ResponseWriter, _ *ehttp.Request) { res.String("ok") }
	outer := middleware.Secure(middleware.SecureOptions{XFrameOptions: "DENY"})

	// The inner Secure sets only a CSP: its default frame option does not win.
	rec := ehttptest.Do(fixtures.Handler(ok, outer, middleware.Secure(middleware.SecureOptions{ContentSecurityPolicy: "B"})), fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Frame-Options"), "DENY")
	ehttptest.Eq(t, ownBasis, rec.Header().Get("Content-Security-Policy"), "B")

	// An inner option set on purpose wins.
	rec = ehttptest.Do(fixtures.Handler(ok, outer, middleware.Secure(middleware.SecureOptions{XFrameOptions: "SAMEORIGIN"})), fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Frame-Options"), "SAMEORIGIN")

	// No outer Secure: the defaults apply.
	rec = ehttptest.Do(fixtures.Handler(ok, middleware.Secure(middleware.SecureOptions{})), fixtures.Req("GET", "/x"))
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Frame-Options"), "SAMEORIGIN")
	ehttptest.Eq(t, ownBasis, rec.Header().Get("X-Content-Type-Options"), "nosniff")
}
