package ehttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type ResponseWriterInterceptor struct {
	http.ResponseWriter
	Buffer *bytes.Buffer
	Code   int
	Size   int64

	finalized bool
	mu        sync.Mutex
}

// Unwrap returns the real writer under the interceptor.
func (i *ResponseWriterInterceptor) Unwrap() http.ResponseWriter {
	return i.ResponseWriter
}

func (i *ResponseWriterInterceptor) WriteHeader(code int) {
	if isInformational(code) {
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.finalized {
		return
	}

	i.Code = code
}

func (i *ResponseWriterInterceptor) Write(b []byte) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.finalized {
		return 0, fmt.Errorf("response body already written")
	}

	i.Size += int64(len(b))

	return i.Buffer.Write(b)
}

func (i *ResponseWriterInterceptor) SendResponse(res *ResponseWriter) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.finalized {
		return
	}

	i.finalized = true

	if i.Code == 0 {
		i.Code = StatusOK
	}

	res.code = i.Code
	res.sent = true

	i.ResponseWriter.WriteHeader(i.Code)
	i.ResponseWriter.Write(i.Buffer.Bytes())
}

// WithResponseWriter creates a new ResponseWriter with an interceptor as
// writer. The interceptor captures the response so a middleware can change it
// (for example etag) and then send it with SendResponse.
func WithResponseWriter(res *ResponseWriter) (*ResponseWriter, *ResponseWriterInterceptor) {
	i := &ResponseWriterInterceptor{
		ResponseWriter: res.w,
		Buffer:         &bytes.Buffer{},
		Code:           res.code,
	}

	return &ResponseWriter{
		w:        i,
		request:  res.request,
		req:      res.req,
		code:     res.code,
		template: res.template,
	}, i
}

var _ http.ResponseWriter = (*ResponseWriter)(nil)

type ResponseWriter struct {
	w        http.ResponseWriter
	request  *http.Request
	req      *Request // for the logger; nil in a writer built by hand
	code     int
	template *template.Template
	sent     bool
}

// logger returns the logger of the request, or a no-op logger.
func (res *ResponseWriter) logger() *zap.Logger {
	if res.req == nil {
		return nopLogger
	}

	return res.req.newLogger()
}

// HeadersSent reports that the header was sent through the ResponseWriter, by
// a method or by a native handler (see WrapHandler), or, inside an
// interceptor, that the response was committed to the buffer. It does not track
// writes made through Unwrap.
func (res *ResponseWriter) HeadersSent() bool {
	return res.sent
}

// writeHeader writes the status line one time only.
func (res *ResponseWriter) writeHeader(code int) {
	if res.sent {
		return
	}

	res.sent = true
	res.w.WriteHeader(code)
}

// WriteHeader implements http.ResponseWriter. Only the first call counts.
func (res *ResponseWriter) WriteHeader(code int) {
	if res.finished() || res.sent {
		return
	}

	if isInformational(code) {
		res.w.WriteHeader(code)
		return
	}

	res.code = code
	res.writeHeader(code)
}

// isInformational reports a 1xx status that is not final (RFC 9110 §15.2).
func isInformational(code int) bool {
	return code >= 100 && code <= 199 && code != StatusSwitchingProtocols
}

// Write implements http.ResponseWriter. It sends the header first, with the
// current status code.
func (res *ResponseWriter) Write(b []byte) (int, error) {
	if err := res.request.Context().Err(); err != nil {
		return 0, err
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.writeHeader(res.code)

	return res.w.Write(b)
}

// Flush implements http.Flusher. See FlushError.
func (res *ResponseWriter) Flush() {
	_ = res.FlushError()
}

// FlushError sends the header and flushes the inner writer. It returns
// http.ErrNotSupported if the inner writer cannot flush (for example an
// interceptor). http.ResponseController uses it.
func (res *ResponseWriter) FlushError() error {
	if err := res.request.Context().Err(); err != nil {
		return err
	}

	var flush func() error

	switch w := res.w.(type) {
	case interface{ FlushError() error }:
		flush = w.FlushError
	case http.Flusher:
		flush = func() error { w.Flush(); return nil }
	default:
		return http.ErrNotSupported
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.writeHeader(res.code)

	return flush()
}

// ReadFrom implements io.ReaderFrom, so io.Copy keeps the fast path of the
// inner writer.
func (res *ResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if err := res.request.Context().Err(); err != nil {
		return 0, err
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.writeHeader(res.code)

	if rf, ok := res.w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	return io.Copy(struct{ io.Writer }{res.w}, r)
}

// Hijack implements http.Hijacker.
func (res *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := res.w.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}

	conn, rw, err := h.Hijack()
	if err == nil {
		res.sent = true
	}

	return conn, rw, err
}

// Unwrap returns the inner writer, for http.ResponseController. A write
// through it is not tracked by HeadersSent.
func (res *ResponseWriter) Unwrap() http.ResponseWriter {
	return res.w
}

func (res *ResponseWriter) finished() bool {
	cancelled := errors.Is(res.request.Context().Err(), context.Canceled)
	timeout := errors.Is(res.request.Context().Err(), context.DeadlineExceeded)

	return cancelled || timeout
}

func (res *ResponseWriter) Header() http.Header {
	if res.finished() {
		return http.Header{}
	}

	return res.w.Header()
}

func (res *ResponseWriter) StatusCode() int {
	return res.code
}

func (res *ResponseWriter) Status(code int) {
	if res.finished() || res.sent {
		return
	}

	res.code = code
}

func (res *ResponseWriter) Append(name, value string) {
	if res.finished() {
		return
	}

	res.Header().Set(name, value)
}

func (res *ResponseWriter) Vary(value string) {
	if res.finished() {
		return
	}

	value = strings.TrimSpace(value)

	if value == "" {
		return
	}

	lines := res.Header().Values(HeaderVary)

	if varyHas(lines, value) {
		return
	}

	if len(lines) == 0 {
		res.Header().Set(HeaderVary, value)
		return
	}

	names := make([]string, 0, len(lines)+1)

	for _, line := range lines {
		for _, name := range strings.Split(line, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}

	res.Header().Set(HeaderVary, strings.Join(append(names, value), ", "))
}

// equalASCIIFold compares two field names, ignoring ASCII case only.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}

	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}

	return c
}

// varyHas reports if a Vary field line lists name. It does not allocate.
func varyHas(lines []string, name string) bool {
	for _, line := range lines {
		for line != "" {
			var part string

			part, line, _ = strings.Cut(line, ",")

			if equalASCIIFold(strings.TrimSpace(part), name) {
				return true
			}
		}
	}

	return false
}

// Cookie adds a Set-Cookie field line. An invalid cookie is not sent.
func (res *ResponseWriter) Cookie(cookie *http.Cookie) {
	if res.finished() {
		return
	}

	if cookie == nil {
		res.logger().Error("nil cookie")
		return
	}

	if err := cookie.Valid(); err != nil {
		res.logger().Error("invalid cookie", zap.String("name", cookie.Name), zap.Error(err))
		return
	}

	res.Header().Add(HeaderSetCookie, cookie.String())
}

// ClearCookie asks the client to delete the cookie name (Path "/").
func (res *ResponseWriter) ClearCookie(name string) {
	// The cookie is deleted, so it needs no Secure, HttpOnly or SameSite.
	res.Cookie(&http.Cookie{Name: name, Path: "/", MaxAge: -1}) //nolint:gosec // G124
}

func (res *ResponseWriter) Location(value string) {
	res.Header().Set(HeaderLocation, value)
}

func (res *ResponseWriter) End() {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.writeHeader(res.code)
}

func (res *ResponseWriter) SendStatus(code int) {
	if res.finished() {
		return
	}

	if !res.sent {
		res.code = code
	}

	res.writeHeader(code)
}

func (res *ResponseWriter) Send(v []byte) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(v)))

	res.writeHeader(res.code)
	_, _ = res.w.Write(v)
}

func (res *ResponseWriter) Render(name string, data interface{}) {
	if res.finished() {
		return
	}

	buf := &bytes.Buffer{}

	if err := res.template.ExecuteTemplate(buf, name, data); err != nil {
		panic(err)
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(buf.Len()))
	res.Header().Set(HeaderContentType, MIMETextHTMLCharsetUTF8)

	res.writeHeader(res.code)
	_, _ = buf.WriteTo(res.w)
}

func (res *ResponseWriter) Error(e error) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusInternalServerError
	}

	bytes := []byte(e.Error())

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(bytes)))
	res.Header().Set(HeaderContentType, MIMETextPlainCharsetUTF8)

	res.writeHeader(res.code)
	_, _ = res.w.Write(bytes)
}

func (res *ResponseWriter) Redirect(value string) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusTemporaryRedirect
	}

	res.Header().Del(HeaderContentLength)
	res.Header().Del(HeaderContentType)

	res.Location(value)

	res.writeHeader(res.code)
}

func (res *ResponseWriter) String(v string) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	b := []byte(v)

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(b)))
	res.Header().Set(HeaderContentType, MIMETextPlainCharsetUTF8)

	res.writeHeader(res.code)
	_, _ = res.w.Write(b)
}

func (res *ResponseWriter) JSON(v interface{}) {
	if res.finished() {
		return
	}

	bytes, err := json.Marshal(v)

	if err != nil {
		panic(err)
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(bytes)))
	res.Header().Set(HeaderContentType, MIMEApplicationJSONCharsetUTF8)

	res.writeHeader(res.code)
	_, _ = res.w.Write(bytes)
}

func (res *ResponseWriter) SendFile(name string) {
	if res.finished() {
		return
	}

	if hasDotDot(name) {
		res.logger().Error("path traversal refused", zap.String("filename", name))
		res.SendStatus(StatusNotFound)
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	file, err := os.Open(name)

	if err != nil {
		res.logger().Error("file not found", zap.String("filename", name))
		res.SendStatus(StatusNotFound)
		return
	}

	defer file.Close()

	file_stat, err := file.Stat()

	if err != nil || file_stat.IsDir() {
		res.logger().Error("invalid file", zap.String("filename", name))
		res.SendStatus(StatusNotFound)
		return
	}

	file_size := file_stat.Size()
	file_modtime := file_stat.ModTime()
	file_name := filepath.Base(name)

	hash := crc32.ChecksumIEEE([]byte(file_name + file_modtime.String() + strconv.FormatInt(file_size, 10)))
	etag := fmt.Sprintf(`W/"%d-%08x"`, file_size, hash)

	res.Header().Set(HeaderAcceptRanges, "bytes")
	res.Header().Set(HeaderEtag, etag)
	res.Header().Set(HeaderLastModified, file_modtime.UTC().Format(http.TimeFormat))

	if fileIsNotModified(res, etag, file_modtime) {
		res.SendStatus(StatusNotModified)
		return
	}

	req := res.request

	if ims := req.Header.Get(HeaderIfModifiedSince); ims != "" {
		if t, err := time.Parse(http.TimeFormat, ims); err == nil && !file_modtime.After(t) {
			res.SendStatus(StatusNotModified)
			return
		}
	}

	{
		file_mimetype := mime.TypeByExtension(filepath.Ext(name))

		if file_mimetype == "" {
			var buf [512]byte
			n, _ := io.ReadFull(file, buf[:])
			file_mimetype = http.DetectContentType(buf[:n])
			_, err := file.Seek(0, io.SeekStart)

			if err != nil {
				res.logger().Error("seeker can't seek", zap.Error(err))
				res.SendStatus(StatusInternalServerError)
				return
			}
		}

		res.Header().Set(HeaderContentType, file_mimetype)
	}

	if req.Method == MethodHead {
		res.Header().Set(HeaderContentLength, strconv.FormatInt(file_size, 10))
		res.SendStatus(res.code)
		return
	}

	header_range := req.Header.Get(HeaderRange)

	if header_range != "" {
		start, end, code := fileParseRange(header_range, file_size)

		if code != http.StatusPartialContent {
			res.SendStatus(code)
			return
		}

		res.Header().Set(HeaderContentRange, fmt.Sprintf("bytes %d-%d/%d", start, end, file_size))
		res.Header().Set(HeaderContentLength, strconv.FormatInt(end-start+1, 10))

		res.Status(http.StatusPartialContent)

		res.writeHeader(res.code)
		file.Seek(start, io.SeekStart)
		_, _ = io.CopyN(res.w, file, end-start+1)
		return
	}

	res.Header().Set(HeaderContentLength, strconv.FormatInt(file_size, 10))

	res.writeHeader(res.code)
	_, _ = file.WriteTo(res.w)
}

// hasDotDot reports if any element of the path is "..".
func hasDotDot(name string) bool {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}

	return false
}

func fileIsNotModified(res *ResponseWriter, etag string, modTime time.Time) bool {
	req := res.request

	if inm := req.Header.Get(HeaderIfNoneMatch); inm != "" && inm == etag {
		return true
	}

	if ims := req.Header.Get(HeaderIfModifiedSince); ims != "" {
		if t, err := time.Parse(http.TimeFormat, ims); err == nil && !modTime.After(t) {
			return true
		}
	}

	return false
}

func fileParseRange(rangeHeader string, fileSize int64) (start, end int64, statusCode int) {
	var s, e int64

	if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &s, &e); err != nil {
		return 0, 0, StatusRequestedRangeNotSatisfiable
	}

	if s < 0 || s >= fileSize {
		return 0, 0, StatusRequestedRangeNotSatisfiable
	}

	if e <= 0 || e >= fileSize {
		e = fileSize - 1
	}

	if s > e {
		return 0, 0, StatusRequestedRangeNotSatisfiable
	}

	return s, e, StatusPartialContent
}
