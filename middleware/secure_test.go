package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"github.com/EqualSoftware/ehttp/internal/fixtures"
	"github.com/EqualSoftware/ehttp/middleware"
)

func TestSecureDefaults(t *testing.T) {
	t.Parallel()

	for name, opt := range map[string]middleware.SecureOptions{
		"DefaultSecureConfig": middleware.DefaultSecureConfig,
		"zero options":        {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Secure(opt)), fixtures.Req("GET", "/x"))

			ehttptest.Eq(t, "OWASP Secure Headers", rec.Header().Get("X-Content-Type-Options"), "nosniff")
			ehttptest.Eq(t, "OWASP Secure Headers", rec.Header().Get("X-Frame-Options"), "SAMEORIGIN")
			ehttptest.Eq(t, "middleware contract", rec.Header().Get("Strict-Transport-Security"), "")
			ehttptest.Eq(t, "middleware contract", rec.Header().Get("Content-Security-Policy"), "")
			ehttptest.Eq(t, "middleware contract", rec.Header().Get("Referrer-Policy"), "")
			ehttptest.Eq(t, "middleware contract (next called)", rec.Body.String(), "ok")
		})
	}
}

// C9: OWASP recommends `X-XSS-Protection: 0`; the "1; mode=block" auditor
// was removed from browsers and had its own information leaks.
func TestSecureXSSProtectionDefault(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "C9", func(t testing.TB) {
		rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Secure(middleware.DefaultSecureConfig)), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "C9 (OWASP Secure Headers)", rec.Header().Get("X-XSS-Protection"), "0")
	})
}

func TestSecureCustomValues(t *testing.T) {
	t.Parallel()

	t.Run("overrides", func(t *testing.T) {
		t.Parallel()

		h := fixtures.Handler(fixtures.OK, middleware.Secure(middleware.SecureOptions{
			XSSProtection:      "0",
			ContentTypeNosniff: "nosniff",
			XFrameOptions:      "DENY",
			ReferrerPolicy:     "no-referrer",
		}))
		rec := ehttptest.Do(h, fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "middleware contract", rec.Header().Get("X-XSS-Protection"), "0")
		ehttptest.Eq(t, "middleware contract", rec.Header().Get("X-Frame-Options"), "DENY")
		ehttptest.Eq(t, "middleware contract", rec.Header().Get("Referrer-Policy"), "no-referrer")
	})

	t.Run("CSP enforced vs report-only", func(t *testing.T) {
		t.Parallel()

		enforce := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Secure(middleware.SecureOptions{ContentSecurityPolicy: "default-src 'self'"})), fixtures.Req("GET", "/x"))
		report := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Secure(middleware.SecureOptions{ContentSecurityPolicy: "default-src 'self'", CSPReportOnly: true})), fixtures.Req("GET", "/x"))

		ehttptest.Eq(t, "CSP Level 3 §3", enforce.Header().Get("Content-Security-Policy"), "default-src 'self'")
		ehttptest.Eq(t, "CSP Level 3 §3", enforce.Header().Get("Content-Security-Policy-Report-Only"), "")
		ehttptest.Eq(t, "CSP Level 3 §3", report.Header().Get("Content-Security-Policy-Report-Only"), "default-src 'self'")
		ehttptest.Eq(t, "CSP Level 3 §3", report.Header().Get("Content-Security-Policy"), "")
	})
}

func TestSecureHSTS(t *testing.T) {
	t.Parallel()

	const year = 31536000

	tests := []struct {
		name  string
		opt   middleware.SecureOptions
		proto string
		want  string
	}{
		{"https via proxy", middleware.SecureOptions{HSTSMaxAge: year}, "https", "max-age=31536000; includeSubdomains"},
		{"exclude subdomains", middleware.SecureOptions{HSTSMaxAge: year, HSTSExcludeSubdomains: true}, "https", "max-age=31536000"},
		{"preload", middleware.SecureOptions{HSTSMaxAge: year, HSTSPreloadEnabled: true}, "https", "max-age=31536000; includeSubdomains; preload"},
		{"plain http never sets it", middleware.SecureOptions{HSTSMaxAge: year}, "http", ""},
		{"max age zero disables it", middleware.SecureOptions{}, "https", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := ehttptest.Do(fixtures.Handler(fixtures.OK, middleware.Secure(tt.opt)), fixtures.Req("GET", "/x", "X-Forwarded-Proto", tt.proto))

			ehttptest.Eq(t, "RFC 6797 §6.1", rec.Header().Get("Strict-Transport-Security"), tt.want)
		})
	}
}

// RFC 6797 §7.2: HSTS is sent over a secure transport. A request that
// arrives on a TLS listener has no X-Forwarded-Proto and an empty
// URL.Scheme, so Request.Protocol() cannot tell it is https.
func TestSecureHSTSOverDirectTLS(t *testing.T) {
	t.Parallel()

	ehttptest.KnownBug(t, "NB-6", func(t testing.TB) {
		srv := httptest.NewTLSServer(fixtures.Handler(fixtures.OK, middleware.Secure(middleware.SecureOptions{HSTSMaxAge: 31536000})))
		defer srv.Close()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/x", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		ehttptest.Eq(t, "RFC 6797 §7.2", res.StatusCode, http.StatusOK)
		ehttptest.True(t, "RFC 6797 §7.2", res.Header.Get("Strict-Transport-Security") != "",
			"no Strict-Transport-Security on a direct TLS connection")
	})
}

func TestSecureAppliesToRouterErrors(t *testing.T) {
	t.Parallel()

	s := ehttp.NewServer()
	s.Use(middleware.Secure(middleware.DefaultSecureConfig))
	s.GET("/a", fixtures.OK)

	rec := ehttptest.Do(s, fixtures.Req("GET", "/missing"))

	ehttptest.Eq(t, "RFC 9110 §15.5.5", rec.Code, 404)
	ehttptest.Eq(t, "OWASP Secure Headers (also on errors)", rec.Header().Get("X-Content-Type-Options"), "nosniff")
}
