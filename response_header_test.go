package ehttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

// aPanics runs f and returns the panic message, or "" if f did not panic.
func aPanics(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
			if msg == "" {
				msg = "non-string panic"
			}
		}
	}()

	f()

	return ""
}

func TestResponseHeaderPanics(t *testing.T) {
	t.Parallel()

	sent := func(res *ResponseWriter) *ResponseWriter { res.String("x"); return res }

	tests := []struct {
		name  string
		basis string
		want  string // part of the message
		do    func(res *ResponseWriter)
	}{
		{"Set invalid name", "RFC 9110 §5.1", "Bad Name", func(r *ResponseWriter) { r.Set("Bad Name", "v") }},
		{"Set empty name", "RFC 9110 §5.1", "not a token", func(r *ResponseWriter) { r.Set("", "v") }},
		{"Append invalid name", "RFC 9110 §5.1", "not a token", func(r *ResponseWriter) { r.Append("X:Y", "v") }},
		{"Remove invalid name", "RFC 9110 §5.1", "not a token", func(r *ResponseWriter) { r.Remove("X Y") }},
		{"Set value with LF", "RFC 9110 §5.5", "X-A", func(r *ResponseWriter) { r.Set("X-A", "a\nb") }},
		{"Set value with CR", "RFC 9110 §5.5", "X-A", func(r *ResponseWriter) { r.Set("X-A", "a\rb") }},
		{"Set value with NUL", "RFC 9110 §5.5", "X-A", func(r *ResponseWriter) { r.Set("X-A", "a\x00b") }},
		{"Append value with LF", "RFC 9110 §5.5", "X-A", func(r *ResponseWriter) { r.Append("X-A", "a\nb") }},
		{"Vary value with LF", "RFC 9110 §5.5", "Vary", func(r *ResponseWriter) { r.Vary("a\nb") }},
		{"Append empty member", "RFC 9110 §5.6.1.1", "empty member", func(r *ResponseWriter) { r.Append("X-A", "a", "") }},
		{"Append blank member", "RFC 9110 §5.6.1.1", "empty member", func(r *ResponseWriter) { r.Append("X-A", " ") }},
		{"Append Content-Type", "RFC 9110 §8.3", "res.Set(", func(r *ResponseWriter) { r.Append("Content-Type", "a") }},
		{"Append Content-Length", "RFC 9110 §8.6", "res.Set(", func(r *ResponseWriter) { r.Append("content-length", "1") }},
		{"Append Location", "RFC 9110 §10.2.2", "res.Set(", func(r *ResponseWriter) { r.Append("Location", "/") }},
		{"Append ETag", "RFC 9110 §8.8.3", "res.Set(", func(r *ResponseWriter) { r.Append("ETag", "x") }},
		{"Append Last-Modified", "RFC 9110 §8.8.2", "res.Set(", func(r *ResponseWriter) { r.Append("Last-Modified", "x") }},
		{"Append Content-Range", "RFC 9110 §14.4", "res.Set(", func(r *ResponseWriter) { r.Append("Content-Range", "x") }},
		{"Append Retry-After", "RFC 9110 §10.2.3", "res.Set(", func(r *ResponseWriter) { r.Append("Retry-After", "1") }},
		{"Append ACAO", "Fetch §3.3.4", "res.Set(", func(r *ResponseWriter) { r.Append("Access-Control-Allow-Origin", "*") }},
		{"Append ACAC", "Fetch §3.3.4", "res.Set(", func(r *ResponseWriter) { r.Append("Access-Control-Allow-Credentials", "true") }},
		{"Append ACMA", "Fetch §3.3.4", "res.Set(", func(r *ResponseWriter) { r.Append("Access-Control-Max-Age", "1") }},
		{"Set Set-Cookie", "RFC 6265 §3", "res.Cookie(", func(r *ResponseWriter) { r.Set("Set-Cookie", "a=1") }},
		{"Append Set-Cookie", "RFC 6265 §3", "res.Cookie(", func(r *ResponseWriter) { r.Append("Set-Cookie", "a=1") }},
		{"Set Vary", "RFC 9110 §12.5.5", "res.Vary(", func(r *ResponseWriter) { r.Set("Vary", "a") }},
		{"Append Vary", "RFC 9110 §12.5.5", "res.Vary(", func(r *ResponseWriter) { r.Append("vary", "a") }},
		{"Remove Vary", "RFC 9110 §12.5.5", "res.Vary(", func(r *ResponseWriter) { r.Remove("Vary") }},
		{"Set after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Set("X-A", "1") }},
		{"Append after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Append("X-A", "1") }},
		{"Remove after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Remove("X-A") }},
		{"Vary after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Vary("Origin") }},
		{"Cookie after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Cookie(&http.Cookie{Name: "a", Value: "1"}) }},
		{"ClearCookie after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).ClearCookie("a") }},
		{"Status after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Status(404) }},
		{"Location after sent", "strict API: a write after the response is sent panics", "already sent", func(r *ResponseWriter) { sent(r).Location("/") }},
		{"Cookie nil", "strict API: a nil cookie panics", "nil", func(r *ResponseWriter) { r.Cookie(nil) }},
		{"Cookie invalid", "RFC 6265 §4.1.1", "invalid", func(r *ResponseWriter) { r.Cookie(&http.Cookie{Name: "bad name", Value: "1"}) }},
		{"Status 99", "RFC 9110 §15", "99", func(r *ResponseWriter) { r.Status(99) }},
		{"Status 600", "RFC 9110 §15", "600", func(r *ResponseWriter) { r.Status(600) }},
		{"SendStatus 0", "RFC 9110 §15", "outside", func(r *ResponseWriter) { r.SendStatus(0) }},
		{"WriteHeader 700", "RFC 9110 §15", "outside", func(r *ResponseWriter) { r.WriteHeader(700) }},
		{"Status 1xx", "RFC 9110 §15.2", "informational", func(r *ResponseWriter) { r.Status(103) }},
		{"SendStatus 1xx", "RFC 9110 §15.2", "informational", func(r *ResponseWriter) { r.SendStatus(100) }},
		{"String twice", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).String("y") }},
		{"JSON after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).JSON(1) }},
		{"Send after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).Send(nil) }},
		{"Error after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).Error(errors.New("e")) }},
		{"Redirect after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).Redirect("/") }},
		{"SendFile after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).SendFile("x") }},
		{"Render after String", "RFC 9110 §6.4.1", "already sent a body", func(r *ResponseWriter) { sent(r).Render("x", nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, _ := aCountedRes(t)
			msg := aPanics(func() { tt.do(res) })

			ehttptest.True(t, tt.basis, strings.HasPrefix(msg, "ehttp: "), "message %q does not start with %q", msg, "ehttp: ")
			ehttptest.True(t, tt.basis, strings.Contains(msg, tt.want), "message %q has no %q", msg, tt.want)
		})
	}
}

func TestResponseHeaderAppendMerges(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	res.Append("Cache-Control", "a")
	res.Append("Cache-Control", "a")
	res.Append("Link", "x", "y")
	res.Append("Link", "y", "z")
	res.Append("X-Comma", "a, b")
	res.Append("X-Comma", "a, b")

	ehttptest.Eq(t, "RFC 9110 §5.3 (one line)", strings.Join(hc.Header().Values("Cache-Control"), "|"), "a")
	ehttptest.Eq(t, "RFC 9110 §5.6.1", strings.Join(hc.Header().Values("Link"), "|"), "x, y, z")
	ehttptest.Eq(t, "one element is one member", strings.Join(hc.Header().Values("X-Comma"), "|"), "a, b")
}

func TestResponseHeaderAppendMergesRawLines(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	hc.Header().Add("X-A", "1")
	hc.Header().Add("X-A", "2")
	res.Append("X-A", "3")

	ehttptest.Eq(t, "RFC 9110 §5.3", strings.Join(hc.Header().Values("X-A"), "|"), "1, 2, 3")
}

func TestResponseHeaderSetReplaces(t *testing.T) {
	t.Parallel()

	res, hc := aCountedRes(t)
	res.Set("X-A", "1")
	res.Set("x-a", "2")
	res.Remove("X-B")
	res.Set("X-B", "1")
	res.Remove("x-b")

	ehttptest.Eq(t, "Set replaces", strings.Join(hc.Header().Values("X-A"), "|"), "2")
	ehttptest.Eq(t, "Remove deletes", len(hc.Header().Values("X-B")), 0)
}

func TestResponseHeaderVary(t *testing.T) {
	t.Parallel()

	t.Run("comma list twice", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		res.Vary("Origin, Accept")
		res.Vary("Origin, Accept")
		res.Vary("origin", ",, ", "Accept-Encoding")

		ehttptest.Eq(t, "RFC 9110 §12.5.5", strings.Join(hc.Header().Values(HeaderVary), "|"), "Origin, Accept, Accept-Encoding")
	})

	t.Run("star replaces", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		res.Vary("Origin")
		res.Vary("*")
		res.Vary("Accept")

		ehttptest.Eq(t, "RFC 9110 §12.5.5", strings.Join(hc.Header().Values(HeaderVary), "|"), "*")
	})

	t.Run("star in a list", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		res.Vary("Origin, *, Accept")

		ehttptest.Eq(t, "RFC 9110 §12.5.5", strings.Join(hc.Header().Values(HeaderVary), "|"), "*")
	})
}

func TestResponseHeaderContentTypeKept(t *testing.T) {
	t.Parallel()

	rec := aRes(func(res *ResponseWriter, _ *Request) {
		res.Set(HeaderContentType, "application/problem+json")
		res.JSON(1)
	})

	ehttptest.Eq(t, "Echo writeContentType", rec.Header().Get(HeaderContentType), "application/problem+json")

	rec = aRes(func(res *ResponseWriter, _ *Request) { res.JSON(1) })

	ehttptest.Eq(t, "default", rec.Header().Get(HeaderContentType), MIMEApplicationJSONCharsetUTF8)
}

func TestResponseHeaderStatusThenBody(t *testing.T) {
	t.Parallel()

	s := NewServer()
	s.GET("/", func(res *ResponseWriter, _ *Request) {
		res.SendStatus(201)
		res.String("x")
	})

	resp, body := aGetBody(t, aRealServer(t, s).URL+"/")

	ehttptest.Eq(t, "SendStatus then String sends the status and the body", resp.StatusCode, 201)
	ehttptest.Eq(t, "SendStatus then String sends the status and the body", body, "x")
}

func TestResponseHeaderWriteHeader1xx(t *testing.T) {
	t.Parallel()

	res, _ := aCountedRes(t)

	ehttptest.Eq(t, "commit 0d1d6d0", aPanics(func() { res.WriteHeader(103) }), "")
}

func TestResponseHeaderNoPanicAfterClientGone(t *testing.T) {
	t.Parallel()

	res, _ := aCountedRes(t)
	res.String("x")

	ctx, cancel := contextCancelled()
	defer cancel()

	res.request = res.request.WithContext(ctx)

	msg := aPanics(func() {
		res.Set("X-A", "1")
		res.Append("X-A", "1")
		res.Remove("X-A")
		res.Vary("Origin")
		res.Cookie(&http.Cookie{Name: "a", Value: "1"})
		res.Status(404)
		res.String("again")
	})

	ehttptest.Eq(t, "cooperative timeout: a late write after the context ends is dropped", msg, "")
}

func contextCancelled() (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithCancel(context.Background())
	cancel()

	return ctx, cancel
}

func TestValidFieldName(t *testing.T) {
	t.Parallel()

	const tchar = "!#$%&'*+-.^_`|~"

	for c := 0; c < 256; c++ {
		want := strings.IndexByte(tchar, byte(c)) >= 0 && c < 0x80 ||
			c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		ehttptest.Eq(t, "RFC 9110 §5.6.2 (tchar), byte "+strconv.Itoa(c), validFieldName("X"+string([]byte{byte(c)})), want)
	}

	ehttptest.Eq(t, "RFC 9110 §5.6.2 (token is 1*tchar)", validFieldName(""), false)
	ehttptest.Eq(t, "RFC 9110 §5.6.2", validFieldName("Content-Type"), true)
	ehttptest.Eq(t, "RFC 9110 §5.6.2", validFieldName("X Y"), false)
	ehttptest.Eq(t, "RFC 9110 §5.6.2", validFieldName("X:"), false)
}

func TestValidFieldValue(t *testing.T) {
	t.Parallel()

	for c := 0; c < 256; c++ {
		want := c == '\t' || c >= 0x20 && c != 0x7f // SP, VCHAR, obs-text and HTAB
		ehttptest.Eq(t, "RFC 9110 §5.5, byte "+strconv.Itoa(c), validFieldValue("a"+string([]byte{byte(c)})+"b"), want)
	}

	ehttptest.Eq(t, "RFC 9110 §5.5 (empty value)", validFieldValue(""), true)
	ehttptest.Eq(t, "RFC 9110 §5.5", validFieldValue("text/html; charset=utf-8"), true)
	ehttptest.Eq(t, "RFC 9110 §5.5 (obs-text)", validFieldValue("caf\xc3\xa9"), true)
	ehttptest.Eq(t, "RFC 9110 §5.5 (CRLF)", validFieldValue("a\r\nb: c"), false)
}

func TestResponseHeaderOneLine(t *testing.T) {
	t.Parallel()

	const basis = "RFC 9110 §5.3 (a list field is always one line)"

	t.Run("Append without news", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		hc.Header().Add("X-A", "1")
		hc.Header().Add("X-A", "2")
		res.Append("X-A", "1")

		ehttptest.Eq(t, basis, strings.Join(hc.Header().Values("X-A"), "|"), "1, 2")
	})

	t.Run("Vary without news", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		hc.Header().Add(HeaderVary, "Origin")
		hc.Header().Add(HeaderVary, "Accept")
		res.Vary("origin")

		ehttptest.Eq(t, basis, strings.Join(hc.Header().Values(HeaderVary), "|"), "Origin, Accept")
	})

	t.Run("Vary after a junk line", func(t *testing.T) {
		t.Parallel()

		res, hc := aCountedRes(t)
		hc.Header().Set(HeaderVary, " , ")
		res.Vary("d")

		ehttptest.Eq(t, "RFC 9110 §5.6.1.1 (no empty member)", strings.Join(hc.Header().Values(HeaderVary), "|"), "d")
	})
}

func TestResponseWriterInterceptorBodySent(t *testing.T) {
	t.Parallel()

	outer, _ := aCountedRes(t)
	inner, ic := WithResponseWriter(outer)
	inner.String("x")
	ic.SendResponse(outer)

	msg := aPanics(func() { outer.String("y") })

	ehttptest.True(t, "SendResponse sends a body, so a second body panics", strings.Contains(msg, "already sent a body"), "message %q", msg)
}

func TestBootValidation(t *testing.T) {
	t.Parallel()

	const basis = "invalid developer input panics at boot"

	cors := func(o CorsOptions) func() {
		if len(o.Origin) == 0 {
			o.Origin = []string{"*"}
		}

		return func() { Cors(o) }
	}

	tests := []struct {
		name string
		do   func()
		want string
	}{
		{"empty origin", cors(CorsOptions{Origin: []string{""}}), "Origin"},
		{"origin with LF", cors(CorsOptions{Origin: []string{"http://a\nb"}}), "Origin"},
		{"empty method", cors(CorsOptions{Methods: []string{""}}), "Methods"},
		{"method with space", cors(CorsOptions{Methods: []string{"GE T"}}), "Methods"},
		{"empty header", cors(CorsOptions{Headers: []string{""}}), "Headers"},
		{"header with colon", cors(CorsOptions{Headers: []string{"X:Y"}}), "Headers"},
		{"route method", func() { NewServer().router.Handle("GE T", "/", func(*ResponseWriter, *Request) {}) }, "method"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := aPanics(tt.do)

			ehttptest.True(t, basis, strings.HasPrefix(msg, "ehttp: ") && strings.Contains(msg, tt.want), "message %q", msg)
		})
	}
}
