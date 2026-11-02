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
	"slices"
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
	res.bodySent = true

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
	bodySent bool // a body was written; a status line alone is not a body
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
	checkStatus("WriteHeader", code)

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
	res.bodySent = true

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
	res.bodySent = true

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

// Header returns the raw header map. It is the escape hatch that
// http.ResponseWriter requires: nothing is validated, merged or checked here.
// Use Set, Append, Vary, Cookie and Remove for checked changes.
func (res *ResponseWriter) Header() http.Header {
	if res.finished() {
		return http.Header{}
	}

	return res.w.Header()
}

func (res *ResponseWriter) StatusCode() int {
	return res.code
}

// Status sets the final status code of the response. It panics for a code
// outside 200-599 and after the header was sent.
func (res *ResponseWriter) Status(code int) {
	checkFinalStatus("Status", code)

	if res.finished() {
		return
	}

	res.mustNotBeSent("Status", "status")

	res.code = code
}

// checkStatus panics for a code outside 100-599 (RFC 9110 §15).
func checkStatus(method string, code int) {
	if code < 100 || code > 599 {
		panic("ehttp: " + method + ": status code " + strconv.Itoa(code) + " is outside 100-599 (RFC 9110 §15)")
	}
}

// checkFinalStatus is checkStatus and refuses a 1xx: the method sends a final
// status (RFC 9110 §15.2).
func checkFinalStatus(method string, code int) {
	checkStatus(method, code)

	if code < 200 {
		panic("ehttp: " + method + ": status code " + strconv.Itoa(code) + " is informational and not final (RFC 9110 §15.2); use WriteHeader")
	}
}

// mustNotBeSent panics if the header was sent: a change would be lost.
func (res *ResponseWriter) mustNotBeSent(method, name string) {
	if res.sent {
		panic("ehttp: " + method + ": the header was already sent (" + name + ", RFC 9110 §6.4.1); change it before the first send")
	}
}

// mustHaveNoBody panics if a body was already written.
func (res *ResponseWriter) mustHaveNoBody(what string) {
	if res.bodySent {
		panic("ehttp: " + what + ": response already sent (RFC 9110 §6.4.1); a response has one body")
	}
}

// singletons are the fields that allow one value only: Append refuses them.
var singletons = [...]struct{ name, basis string }{
	{HeaderContentType, "RFC 9110 §8.3"},
	{HeaderContentLength, "RFC 9110 §8.6"},
	{HeaderLocation, "RFC 9110 §10.2.2"},
	{HeaderEtag, "RFC 9110 §8.8.3"},
	{HeaderLastModified, "RFC 9110 §8.8.2"},
	{HeaderContentRange, "RFC 9110 §14.4"},
	{HeaderRetryAfter, "RFC 9110 §10.2.3"},
	{HeaderAccessControlAllowOrigin, "Fetch §3.3.4"},
	{HeaderAccessControlAllowCredentials, "Fetch §3.3.4"},
	{HeaderAccessControlMaxAge, "Fetch §3.3.4"},
}

// checkName panics if name is not a field name (RFC 9110 §5.1).
func checkName(method, name string) {
	if !validFieldName(name) {
		panic("ehttp: " + method + ": header name " + strconv.Quote(name) + " is not a token (RFC 9110 §5.1)")
	}
}

// tcharTable marks the token characters (RFC 9110 §5.6.2): ALPHA, DIGIT and
// one of !#$%&'*+-.^_`|~.
var tcharTable = func() (t [256]bool) {
	for c := 0; c < 256; c++ {
		t[c] = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c < 0x80 && strings.IndexByte("!#$%&'*+-.^_`|~", byte(c)) >= 0
	}

	return t
}()

// valueTable marks the bytes of a field value (RFC 9110 §5.5): VCHAR, obs-text,
// SP and HTAB. CR, LF, NUL, other control bytes and DEL are not marked.
var valueTable = func() (t [256]bool) {
	for c := 0; c < 256; c++ {
		t[c] = c == '\t' || c >= 0x20 && c != 0x7f
	}

	return t
}()

// validFieldName reports if name is a token (RFC 9110 §5.6.2). It does not
// allocate.
func validFieldName(name string) bool {
	if name == "" {
		return false
	}

	for i := 0; i < len(name); i++ {
		if !tcharTable[name[i]] {
			return false
		}
	}

	return true
}

// validFieldValue reports if value fits RFC 9110 §5.5. It does not allocate.
func validFieldValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if !valueTable[value[i]] {
			return false
		}
	}

	return true
}

// checkValue panics if value has a byte that a field value cannot have
// (RFC 9110 §5.5).
func checkValue(method, name, value string) {
	if !validFieldValue(value) {
		panic("ehttp: " + method + ": header " + name + " has an invalid value " + strconv.Quote(value) + " (RFC 9110 §5.5)")
	}
}

// checkOwned panics for the fields that have their own method.
func checkOwned(method, name string) {
	switch {
	case equalASCIIFold(name, HeaderSetCookie):
		panic("ehttp: " + method + ": header Set-Cookie has its own method, use Cookie (RFC 6265 §3)")
	case equalASCIIFold(name, HeaderVary):
		panic("ehttp: " + method + ": header Vary has its own method, use Vary (RFC 9110 §12.5.5)")
	}
}

// Set replaces all values of the field name with value. It panics for an
// invalid name or value, for Set-Cookie and Vary (use Cookie and Vary) and
// after the header was sent. It does nothing after the client is gone.
func (res *ResponseWriter) Set(name, value string) {
	checkName("Set", name)
	checkValue("Set", name, value)
	checkOwned("Set", name)

	if res.finished() {
		return
	}

	res.mustNotBeSent("Set", name)

	res.w.Header().Set(name, value)
}

// Append adds members to the list field name. All members go in one field line
// and a member that is already there (same bytes) is skipped. Each element of
// values is one member: nothing is split. It panics for an empty member, for
// an invalid name or value, for a singleton field such as Content-Type (use
// Set), for Set-Cookie and Vary, and after the header was sent. It does nothing
// after the client is gone.
//
// Members are compared as whole strings. A value with a quoted comma, such as
// `rel="x, y"`, is not parsed.
func (res *ResponseWriter) Append(name string, values ...string) {
	checkName("Append", name)
	checkOwned("Append", name)

	for _, singleton := range singletons {
		if equalASCIIFold(name, singleton.name) {
			panic("ehttp: Append: header " + name + " takes one value, use Set (" + singleton.basis + ")")
		}
	}

	for _, value := range values {
		checkValue("Append", name, value)

		if strings.TrimSpace(value) == "" {
			panic("ehttp: Append: header " + name + " has an empty member (RFC 9110 §5.6.1.1)")
		}
	}

	if res.finished() {
		return
	}

	res.mustNotBeSent("Append", name)

	header := res.w.Header()
	lines := header.Values(name)
	line := joinLines(lines)
	multi := len(lines) > 1 // always leave one field line

	if line == "" && len(values) == 1 {
		header.Set(name, values[0])
		return
	}

	if !multi && !appendNews(line, values) {
		return
	}

	var out strings.Builder

	out.Grow(len(line) + appendSize(values))
	out.WriteString(line)

	for i, value := range values {
		if isOldMember(line, values, i) {
			continue
		}

		if out.Len() > 0 {
			out.WriteString(", ")
		}

		out.WriteString(value)
	}

	header.Set(name, out.String())
}

// isOldMember reports if values[i] is in the line or is a copy of an earlier
// value.
func isOldMember(line string, values []string, i int) bool {
	return listHas(line, values[i]) || slices.Contains(values[:i], values[i])
}

// appendNews reports if one value is not in the line yet.
func appendNews(line string, values []string) bool {
	for i := range values {
		if !isOldMember(line, values, i) {
			return true
		}
	}

	return false
}

// Remove deletes the field name. It panics for an invalid name, for Vary (use
// Vary) and after the header was sent. It does nothing after the client is
// gone.
func (res *ResponseWriter) Remove(name string) {
	checkName("Remove", name)

	if equalASCIIFold(name, HeaderVary) {
		panic("ehttp: Remove: header Vary has its own method, use Vary (RFC 9110 §12.5.5)")
	}

	if res.finished() {
		return
	}

	res.mustNotBeSent("Remove", name)

	res.w.Header().Del(name)
}

// appendSize is the size of the members with their separators.
func appendSize(values []string) (n int) {
	for _, value := range values {
		n += len(value) + 2
	}

	return n
}

// joinLines joins the field lines of a list field (RFC 9110 §5.3). It does not
// allocate for one line.
func joinLines(lines []string) string {
	if len(lines) == 1 {
		return lines[0]
	}

	return strings.Join(lines, ", ")
}

// listHas reports if a list field value has member (byte-exact, around
// optional whitespace). It does not allocate.
func listHas(line, member string) bool {
	for {
		line = skipOWS(line)

		if len(line) >= len(member) && line[:len(member)] == member {
			if rest := skipOWS(line[len(member):]); rest == "" || rest[0] == ',' {
				return true
			}
		}

		i := strings.IndexByte(line, ',')

		if i < 0 {
			return false
		}

		line = line[i+1:]
	}
}

// skipOWS removes leading SP and HTAB (RFC 9110 §5.6.3).
func skipOWS(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}

	return s
}

// Vary adds names to the Vary field (RFC 9110 §12.5.5). It is the only way to
// change Vary. Each argument can list names with commas. An empty name is
// skipped, a name that is already there (ASCII case-insensitive) is skipped
// and "*" replaces all. Vary stays one field line. It panics for an invalid
// value and after the header was sent. It does nothing after the client is
// gone.
func (res *ResponseWriter) Vary(names ...string) {
	for _, value := range names {
		checkValue("Vary", HeaderVary, value)
	}

	if res.finished() {
		return
	}

	res.mustNotBeSent("Vary", HeaderVary)

	header := res.w.Header()
	lines := header.Values(HeaderVary)
	old := joinLines(lines)
	line := old

	for _, value := range names {
		line = varyAdd(line, value)
	}

	if line != old || len(lines) > 1 {
		header.Set(HeaderVary, line)
	}
}

// varyAdd adds the names of value to the Vary line.
func varyAdd(line, value string) string {
	for value != "" {
		var name string

		name, value, _ = strings.Cut(value, ",")

		name = strings.TrimSpace(name)

		if name == "" || varyHasLine(line, "*") {
			continue
		}

		switch {
		case name == "*":
			line = "*"
		case varyHasLine(line, name):
		default:
			if line = varyNormalize(line); line == "" {
				line = name
			} else {
				line += ", " + name
			}
		}
	}

	return line
}

// varyNormalize writes the names of a Vary line as "A, B", without empty names.
func varyNormalize(line string) string {
	if !strings.Contains(line, ",") {
		return strings.TrimSpace(line)
	}

	var b strings.Builder

	for line != "" {
		var name string

		name, line, _ = strings.Cut(line, ",")

		if name = strings.TrimSpace(name); name == "" {
			continue
		}

		if b.Len() > 0 {
			b.WriteString(", ")
		}

		b.WriteString(name)
	}

	return b.String()
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

// varyHasLine reports if a Vary field line lists name. It does not allocate.
func varyHasLine(line, name string) bool {
	for line != "" {
		var part string

		part, line, _ = strings.Cut(line, ",")

		if equalASCIIFold(strings.TrimSpace(part), name) {
			return true
		}
	}

	return false
}

// Cookie adds a Set-Cookie field line (RFC 6265 §3). It is the only field that
// can have many lines. It panics for a nil or invalid cookie and after the
// header was sent. It does nothing after the client is gone.
func (res *ResponseWriter) Cookie(cookie *http.Cookie) {
	if cookie == nil {
		panic("ehttp: Cookie: the cookie is nil")
	}

	if err := cookie.Valid(); err != nil {
		panic("ehttp: Cookie: cookie " + strconv.Quote(cookie.Name) + " is invalid (RFC 6265 §4.1.1): " + err.Error())
	}

	if res.finished() {
		return
	}

	res.mustNotBeSent("Cookie", HeaderSetCookie)

	res.w.Header().Add(HeaderSetCookie, cookie.String())
}

// ClearCookie asks the client to delete the cookie name (Path "/").
func (res *ResponseWriter) ClearCookie(name string) {
	// The cookie is deleted, so it needs no Secure, HttpOnly or SameSite.
	res.Cookie(&http.Cookie{Name: name, Path: "/", MaxAge: -1}) //nolint:gosec // G124
}

// Location sets the Location field. See Set.
func (res *ResponseWriter) Location(value string) {
	res.Set(HeaderLocation, value)
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

// SendStatus sends the status line and no body. It panics for a code outside
// 200-599 and after the header was sent.
func (res *ResponseWriter) SendStatus(code int) {
	checkFinalStatus("SendStatus", code)

	if res.finished() {
		return
	}

	res.mustNotBeSent("SendStatus", "status")

	res.code = code
	res.writeHeader(code)
}

func (res *ResponseWriter) Send(v []byte) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("Send")

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(v)))

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(v)
}

func (res *ResponseWriter) Render(name string, data interface{}) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("Render")

	buf := &bytes.Buffer{}

	if err := res.template.ExecuteTemplate(buf, name, data); err != nil {
		panic(err)
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(buf.Len()))
	res.defaultContentType(MIMETextHTMLCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = buf.WriteTo(res.w)
}

// defaultContentType sets Content-Type only if the handler did not set it.
func (res *ResponseWriter) defaultContentType(value string) {
	if h := res.Header(); h.Get(HeaderContentType) == "" {
		h.Set(HeaderContentType, value)
	}
}

func (res *ResponseWriter) Error(e error) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("Error")

	if res.code == 0 {
		res.code = StatusInternalServerError
	}

	bytes := []byte(e.Error())

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(bytes)))
	res.defaultContentType(MIMETextPlainCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(bytes)
}

func (res *ResponseWriter) Redirect(value string) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("Redirect")

	if res.code == 0 {
		res.code = StatusTemporaryRedirect
	}

	res.Header().Del(HeaderContentLength)
	res.Header().Del(HeaderContentType)

	res.Location(value)

	res.writeHeader(res.code)
	res.bodySent = true
}

func (res *ResponseWriter) String(v string) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("String")

	if res.code == 0 {
		res.code = StatusOK
	}

	b := []byte(v)

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(b)))
	res.defaultContentType(MIMETextPlainCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(b)
}

func (res *ResponseWriter) JSON(v interface{}) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("JSON")

	bytes, err := json.Marshal(v)

	if err != nil {
		panic(err)
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(bytes)))
	res.defaultContentType(MIMEApplicationJSONCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(bytes)
}

func (res *ResponseWriter) SendFile(name string) {
	if res.finished() {
		return
	}

	res.mustHaveNoBody("SendFile")

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
		res.bodySent = true
		file.Seek(start, io.SeekStart)
		_, _ = io.CopyN(res.w, file, end-start+1)
		return
	}

	res.Header().Set(HeaderContentLength, strconv.FormatInt(file_size, 10))

	res.writeHeader(res.code)
	res.bodySent = true
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
