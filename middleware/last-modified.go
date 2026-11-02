package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

func LastModified(last_modified time.Time) ehttp.MiddlewareHandlerFunc {
	last_modified = last_modified.UTC().Truncate(time.Second)

	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			method := req.Method()

			if method != ehttp.MethodGet && method != ehttp.MethodHead {
				next.ServeHTTP(res, req)
				return
			}

			res.Set(ehttp.HeaderLastModified, last_modified.Format(http.TimeFormat))

			since, ok := ifModifiedSince(req)

			if !ok || last_modified.After(since) {
				next.ServeHTTP(res, req)
				return
			}

			// The handler runs first: a server ignores a precondition when the
			// response without it is not 2xx (RFC 9110 §13.2.1). The writer
			// decides at the first status, and does not buffer the body.
			n := &notModified{res: res}
			restore := hook.WrapWriter(res, func(w http.ResponseWriter) http.ResponseWriter {
				n.ResponseWriter = w
				return n
			})
			defer restore()

			next.ServeHTTP(res, req)

			// A handler that wrote nothing has a 2xx response: 304.
			if !res.HeadersSent() && res.StatusCode() < ehttp.StatusMultipleChoices {
				res.SendStatus(ehttp.StatusNotModified)
			}
		}
	}
}

// notModified turns the first 2xx status into 304 and drops the body that the
// handler writes after it. Any other status passes through unchanged. A
// response has no content in a 304 (RFC 9110 §15.4.5), and the handler does
// not know about the condition, so the dropped writes are not an error.
type notModified struct {
	http.ResponseWriter
	res     *ehttp.ResponseWriter // reports the wire status to the middleware outside
	decided bool
	drop    bool
}

func (n *notModified) WriteHeader(code int) {
	if n.decided {
		return
	}

	if code >= 100 && code <= 199 && code != ehttp.StatusSwitchingProtocols {
		n.ResponseWriter.WriteHeader(code) // interim: not the final status
		return
	}

	n.decided = true

	if code >= ehttp.StatusOK && code < ehttp.StatusMultipleChoices {
		code = ehttp.StatusNotModified
		n.drop = true
		hook.SetStatus(n.res, code)
	}

	if code == ehttp.StatusNotModified {
		h := n.Header()
		h.Del(ehttp.HeaderContentLength)
		h.Del(ehttp.HeaderContentType)
	}

	n.ResponseWriter.WriteHeader(code)
}

func (n *notModified) Write(b []byte) (int, error) {
	n.WriteHeader(ehttp.StatusOK)

	if n.drop {
		return len(b), nil
	}

	return n.ResponseWriter.Write(b)
}

// ReadFrom keeps the fast path of the real writer. After a 304 it reads
// nothing from the source.
func (n *notModified) ReadFrom(r io.Reader) (int64, error) {
	n.WriteHeader(ehttp.StatusOK)

	if n.drop {
		return 0, nil
	}

	if rf, ok := n.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	return io.Copy(struct{ io.Writer }{n.ResponseWriter}, r)
}

// Flush is a no-op for a 304: there is nothing to send after the header.
func (n *notModified) Flush() {
	_ = n.FlushError()
}

func (n *notModified) FlushError() error {
	if n.drop {
		return nil
	}

	return http.NewResponseController(n.ResponseWriter).Flush()
}

// Hijack keeps the behavior of the real writer.
func (n *notModified) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(n.ResponseWriter).Hijack()
}

func (n *notModified) Unwrap() http.ResponseWriter {
	return n.ResponseWriter
}

// ifModifiedSince returns the date of If-Modified-Since. It reports false when
// a recipient MUST ignore the field (RFC 9110 §13.1.3): the request has
// If-None-Match, the field has more than one member, or the value is not an
// HTTP-date in one of the three formats of §5.6.7. A list of members does not
// parse as one date, and a date has no list.
func ifModifiedSince(req *ehttp.Request) (time.Time, bool) {
	h := req.Request.Header

	if len(h.Values(ehttp.HeaderIfNoneMatch)) > 0 {
		return time.Time{}, false
	}

	values := h.Values(ehttp.HeaderIfModifiedSince)

	if len(values) != 1 {
		return time.Time{}, false
	}

	since, err := http.ParseTime(values[0])

	return since, err == nil
}
