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
// buffered and keeps its ETag. An If-Match field answers 412 unless it is "*"
// (strong comparison, RFC 9110 section 13.1.1): the ETag is weak. An If-Match or If-None-Match with an invalid
// syntax gets 400.
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
			w := &etagWriter{i: i, res: res, real: i.Unwrap()}
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
	code    int
	passing bool
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

	if !w.passing && w.i.Buffer.Len()+len(b) > etagBufferLimit {
		w.pass()
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
	w.res.Set(ehttp.HeaderEtag, tag)

	// If-Match comes first and uses the strong comparison (RFC 9110 sections
	// 13.1.1, 13.2.2). The ETag of this middleware is weak, so only "*" matches.
	if im := header.Values("If-Match"); len(im) > 0 && !etag.MatchStrong(im, tag) {
		w.i.Buffer.Reset()
		w.code = ehttp.StatusPreconditionFailed
	} else if etag.Match(header.Values(ehttp.HeaderIfNoneMatch), strings.TrimPrefix(tag, "W/")) {
		w.i.Buffer.Reset()
		w.code = ehttp.StatusNotModified
	}

	w.pass()
}
