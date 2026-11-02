package ehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

const redactBasis = "strict logs hide the value of a sensitive field name (OWASP Logging Cheat Sheet)"

// aServeWith serves one GET /x request through a server with options.
func aServeWith(h HandlerFunc, options ...serverOption) string {
	s := NewServer(options...)
	s.GET("/x", h)

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)

	return aPanics(func() { s.ServeHTTP(httptest.NewRecorder(), r) })
}

func TestStrictRedactsSensitiveValue(t *testing.T) {
	t.Parallel()

	const secret = "Bearer s3cr3t\n"

	do := func(res *ResponseWriter, _ *Request) { res.Set("Authorization", secret); res.String("ok") }

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithMode(Production))
	s.GET("/x", do)
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))

	ehttptest.Eq(t, redactBasis, logs.Len(), 1)
	entry := logs.All()[0]
	ehttptest.Eq(t, redactBasis+" (value field)", entry.ContextMap()["value"], any("***"))
	ehttptest.Eq(t, redactBasis+" (header stays visible)", entry.ContextMap()["header"], any("Authorization"))
	ehttptest.True(t, redactBasis+" (message)", !strings.Contains(entry.Message, "s3cr3t") && strings.Contains(entry.Message, "has the value ***,"), "message %q", entry.Message)

	_, _, msg := aServe(Development, do)
	ehttptest.True(t, redactBasis+" (panic)", !strings.Contains(msg, "s3cr3t") && strings.Contains(msg, "has the value ***,"), "message %q", msg)
}

func TestStrictKeepsPlainValue(t *testing.T) {
	t.Parallel()

	_, logs, _ := aServe(Production, func(res *ResponseWriter, _ *Request) { res.Set("X-A", "a\nb"); res.String("ok") })
	ehttptest.Eq(t, redactBasis, logs.All()[0].ContextMap()["value"], any(`"a\nb"`))
}

func TestWithRedactNamesAdds(t *testing.T) {
	t.Parallel()

	do := func(res *ResponseWriter, _ *Request) { res.Set("X-Pin", "12\n34"); res.String("ok") }

	ehttptest.True(t, redactBasis+" (not in the default list)", strings.Contains(aServeWith(do, WithMode(Development)), `"12\n34"`), "default list hides X-Pin")

	msg := aServeWith(do, WithMode(Development), WithRedactNames("*pin"))
	ehttptest.True(t, redactBasis+" (added)", strings.Contains(msg, "has the value ***,"), "message %q", msg)

	do = func(res *ResponseWriter, _ *Request) { res.Set("Authorization", "a\nb"); res.String("ok") }
	msg = aServeWith(do, WithMode(Development), WithRedactNames("*pin"))
	ehttptest.True(t, redactBasis+" (default stays)", strings.Contains(msg, "has the value ***,"), "message %q", msg)
}

func TestRedactOptionsPanicOnInvalidPattern(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"", "*", "**", "a*b", "a b"} {
		msg := aPanics(func() { WithRedactNames(p) })
		ehttptest.True(t, redactBasis+" (server "+p+")", strings.HasPrefix(msg, "ehttp: an invalid pattern in WithRedactNames: ") && strings.Contains(msg, ". Fix: "), "message %q", msg)

		msg = aPanics(func() { WithClientRedactNames(p) })
		ehttptest.True(t, redactBasis+" (client "+p+")", strings.HasPrefix(msg, "ehttp: an invalid pattern in WithClientRedactNames: ") && strings.Contains(msg, ". Fix: "), "message %q", msg)
	}
}

func TestWithClientRedactNamesAdds(t *testing.T) {
	t.Parallel()

	c := NewClient(WithClientRedactNames("*pin"), WithClientRedactNames("tenant"))
	ehttptest.True(t, redactBasis+" (added)", c.redact.Sensitive("user-pin") && c.redact.Sensitive("Tenant"), "added pattern is missing")
	ehttptest.True(t, redactBasis+" (default stays)", c.redact.Sensitive("Authorization"), "default pattern is missing")
	ehttptest.True(t, redactBasis+" (default client)", !NewClient().redact.Sensitive("tenant"), "pattern leaked to the default list")
}

func TestRequestLogsUseAddedPatterns(t *testing.T) {
	t.Parallel()

	serve := func(options ...serverOption) any {
		l, logs := aObserved()
		s := NewServer(append(options, WithLogger(l))...)
		s.GET("/x", func(res *ResponseWriter, req *Request) { req.Accepts("text/html"); res.String("ok") })

		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
		r.Header.Set(HeaderAccept, "garbage")
		s.ServeHTTP(httptest.NewRecorder(), r)

		for _, e := range logs.All() {
			if v, ok := e.ContextMap()["value"]; ok {
				return v
			}
		}

		return nil
	}

	ehttptest.Eq(t, redactBasis+" (plain)", serve(), any(`"garbage"`))
	ehttptest.Eq(t, redactBasis+" (added)", serve(WithRedactNames("accept")), any("***"))
}
