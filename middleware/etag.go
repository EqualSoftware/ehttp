package middleware

import (
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"

	"github.com/EqualSoftware/ehttp"
	"github.com/EqualSoftware/ehttp/internal/etag"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

// etagBufferLimit is the most bytes that WeakEtag buffers. An ETag needs the
// whole body, so a response over the limit is streamed without an ETag.
const etagBufferLimit = 1 << 20

// WeakEtag adds a weak ETag over the body of a 200 response to GET and HEAD, and
// answers 304 when If-None-Match matches it (weak comparison, RFC 9110
// section 13.1.2). The ETag is over the whole body, so the middleware buffers
// the body up to 1 MiB. A larger response is streamed without an ETag. A
// response that already has an ETag (for example from SendFile) is not
// buffered and keeps its ETag. An If-Match field answers 412 to a 2xx response
// unless it is "*" or lists the strong ETag that the handler set (strong
// comparison, RFC 9110 section 13.1.1). The ETag of this middleware is weak,
// and a streamed response has no strong tag, so a list never matches them. An If-Match or If-None-Match with an invalid syntax gets 400.
func WeakEtag() ehttp.MiddlewareHandlerFunc {
	return func(next ehttp.Handler) ehttp.HandlerFunc {
		return func(res *ehttp.ResponseWriter, req *ehttp.Request) {
			method := req.Method()

			if method != ehttp.MethodGet && method != ehttp.MethodHead {
				next.ServeHTTP(res, req)
				return
			}

			header := req.Request.Header

			if !etag.ValidRequest(header) {
				res.SendStatus(ehttp.StatusBadRequest)
				return
			}

			ires, i := ehttp.WithResponseWriter(res)
			w := &etagWriter{i: i, res: res, real: i.Unwrap(), ifMatch: header.Values(ehttp.HeaderIfMatch)}
			hook.WrapWriter(ires, func(http.ResponseWriter) http.ResponseWriter { return w })

			next.ServeHTTP(ires, req)
			w.finish(header)
		}
	}
}

// etagWriter is the writer of the handler. It keeps a 200 response in the
// buffer of the interceptor until the handler ends or the buffer is full. Any
// other response goes to the real writer at once.
type etagWriter struct {
	i       *ehttp.ResponseWriterInterceptor
	res     *ehttp.ResponseWriter
	real    http.ResponseWriter
	ifMatch []string
	code    int
	passing bool
	discard bool // a 412 replaced the response: drop the next writes
}

func (w *etagWriter) Header() http.Header { return w.real.Header() }

func (w *etagWriter) Unwrap() http.ResponseWriter { return w.real }

func (w *etagWriter) WriteHeader(code int) {
	if w.code != 0 {
		return
	}

	if code >= 100 && code <= 199 && code != ehttp.StatusSwitchingProtocols {
		w.real.WriteHeader(code) // interim: not the final status
		return
	}

	w.code = code

	if code != ehttp.StatusOK || w.Header().Get(ehttp.HeaderEtag) != "" {
		w.pass()
	}
}

func (w *etagWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(ehttp.StatusOK)
	}

	if w.discard {
		return len(b), nil
	}

	if !w.passing && w.i.Buffer.Len()+len(b) > etagBufferLimit {
		w.pass()
	}

	if w.discard {
		return len(b), nil
	}

	if w.passing {
		return w.real.Write(b)
	}

	return w.i.Buffer.Write(b)
}

// ReadFrom keeps the fast path of the real writer after the response is passing.
func (w *etagWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.code == 0 {
		w.WriteHeader(ehttp.StatusOK)
	}

	if w.discard {
		return io.Copy(io.Discard, r)
	}

	if rf, ok := w.real.(io.ReaderFrom); ok && w.passing {
		return rf.ReadFrom(r)
	}

	return io.Copy(struct{ io.Writer }{w}, r)
}

// FlushError commits the response: a flush cannot wait for the whole body. The
// buffered bytes go out, the rest is streamed, and there is no ETag.
func (w *etagWriter) FlushError() error {
	if w.code == 0 {
		w.code = ehttp.StatusOK
	}

	w.pass()

	return http.NewResponseController(w.real).Flush()
}

// pass sends the status and the buffered bytes. The next writes go to the real writer.
func (w *etagWriter) pass() {
	if w.passing {
		return
	}

	// The response is committed. A 2xx with no strong tag that If-Match lists
	// (no tag, a weak tag, a flush or a body over the buffer limit) fails the
	// precondition (RFC 9110 sections 13.1.1, 13.2.1).
	if w.code >= 200 && w.code < 300 && w.failsIfMatch(w.Header().Get(ehttp.HeaderEtag)) {
		w.preconditionFailed()
		return
	}

	w.send()
}

// send commits the status and the buffered bytes.
func (w *etagWriter) send() {
	w.passing = true
	w.i.Code = w.code
	w.i.SendResponse(w.res)
}

// finish ends a response that is still in the buffer.
func (w *etagWriter) finish(header http.Header) {
	if w.passing {
		return
	}

	if w.code == 0 {
		w.code = ehttp.StatusOK
	}

	body := w.i.Buffer.Bytes()

	if len(body) == 0 {
		w.pass()
		return
	}

	tag := fmt.Sprintf(`W/"%d-%08x"`, len(body), crc32.ChecksumIEEE(body))

	if w.failsIfMatch(tag) {
		w.preconditionFailed()
		return
	}

	w.res.Set(ehttp.HeaderEtag, tag)

	if etag.Match(header.Values(ehttp.HeaderIfNoneMatch), strings.TrimPrefix(tag, "W/")) {
		w.i.Buffer.Reset()
		w.code = ehttp.StatusNotModified
	}

	w.pass()
}

// failsIfMatch reports that If-Match is there and does not match the current
// tag by strong comparison (RFC 9110 sections 13.1.1, 13.2.2). Only "*" matches
// a weak tag or an empty one (no tag at all).
func (w *etagWriter) failsIfMatch(tag string) bool {
	return len(w.ifMatch) > 0 && !etag.MatchStrong(w.ifMatch, tag)
}

// preconditionFailed sends an empty 412 in place of the response. The headers
// of the dropped representation go with it, or the length would not match.
func (w *etagWriter) preconditionFailed() {
	h := w.Header()
	h.Del(ehttp.HeaderContentLength)
	h.Del(ehttp.HeaderContentType)
	h.Del(ehttp.HeaderEtag)
	h.Del(ehttp.HeaderContentRange)
	h.Del(ehttp.HeaderContentEncoding)
	h.Del(ehttp.HeaderLastModified)
	h.Del(ehttp.HeaderContentDisposition)

	w.i.Buffer.Reset()
	w.code = ehttp.StatusPreconditionFailed
	w.discard = true
	w.send()
}
