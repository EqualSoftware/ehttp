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

	"go.uber.org/zap"

	"github.com/EqualSoftware/ehttp/internal/etag"
	"github.com/EqualSoftware/ehttp/internal/hook"
)

type ResponseWriterInterceptor struct {
	http.ResponseWriter
	Buffer *bytes.Buffer
	Code   int
	Size   int64

	inner     *ResponseWriter // the writer of the handler: its owned fields
	base      uint32          // owned sequence of the outer writer when made
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

	if i.inner != nil {
		res.mergeOwned(i.inner, i.base)
	}

	res.restoreOwned()
	res.sent = true
	res.bodySent = true

	i.ResponseWriter.WriteHeader(i.Code)
	// The write error means the client is gone. There is nobody to tell.
	_, _ = i.ResponseWriter.Write(i.Buffer.Bytes())
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

	inner := &ResponseWriter{
		w:        i,
		request:  res.request,
		req:      res.req,
		code:     res.code,
		template: res.template,
		mode:     res.mode,
	}
	inner.inheritOwned(res)
	i.inner = inner
	i.base = res.ownedSeq

	return inner, i
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
	mode     Mode // the mode of the server: how a strict error ends

	// owned are the fields that a middleware set before the handler (see Own).
	// ownedBuf keeps the first ones without an allocation (CORS and Secure fit).
	owned    []ownedField
	ownedSeq uint32
	ownedBuf [ownedInline]ownedField
}

func init() {
	hook.WrapWriter = func(v any, wrap func(http.ResponseWriter) http.ResponseWriter) func() {
		res := hookWriter("hook.WrapWriter", v)
		old := res.w
		res.w = wrap(old)

		return func() { res.w = old }
	}
	hook.SetStatus = func(v any, code int) { hookWriter("hook.SetStatus", v).code = code }
	hook.BootPanic = bootPanic
}

// hookWriter returns v as a *ResponseWriter. Any other value, also a nil
// one, panics at boot with the message format.
func hookWriter(where string, v any) *ResponseWriter {
	res, ok := v.(*ResponseWriter)
	if !ok || res == nil {
		bootPanic(where, "value of type "+fmt.Sprintf("%T", v), "it is not a non-nil *ehttp.ResponseWriter",
			"pass the *ehttp.ResponseWriter that the handler gets")
	}

	return res
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

	res.restoreOwned()

	res.sent = true
	res.w.WriteHeader(code)
}

// WriteHeader implements http.ResponseWriter. Only the first call counts.
func (res *ResponseWriter) WriteHeader(code int) {
	code = res.checkStatus("WriteHeader", code)

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

// StatusCode returns the status of the response. It is 200 until a handler
// sets another one.
func (res *ResponseWriter) StatusCode() int {
	if res.code == 0 {
		return StatusOK
	}

	return res.code
}

// Status sets the final status code of the response. A code outside 200-599
// and a call after the header was sent are mistakes: Development panics,
// Production logs and uses 500, or ignores the call after the send.
func (res *ResponseWriter) Status(code int) {
	code = res.checkFinalStatus("Status", code)

	if res.finished() {
		return
	}

	if !res.unlessSent("Status", "") {
		return
	}

	res.code = code
}

// checkStatus returns code. For a code outside 100-599 (RFC 9110 §15) it
// fails and returns 500.
func (res *ResponseWriter) checkStatus(method string, code int) int {
	if code >= 100 && code <= 599 {
		return code
	}

	n := strconv.Itoa(code)

	res.fail(strictError{
		level: zap.ErrorLevel,
		rule:  "status code outside 100-599",
		what:  "res." + method + "(" + n + ")",
		why:   "status code " + n + " is outside 100-599 (RFC 9110 §15)",
		fix:   "use a status from 100 to 599, for example res." + method + "(ehttp.StatusOK)",
	})

	return StatusInternalServerError
}

// checkFinalStatus is checkStatus and refuses a 1xx: the method sends a final
// status (RFC 9110 §15.2).
func (res *ResponseWriter) checkFinalStatus(method string, code int) int {
	if code = res.checkStatus(method, code); code >= 200 {
		return code
	}

	n := strconv.Itoa(code)

	res.fail(strictError{
		level: zap.ErrorLevel,
		rule:  "informational status is not final",
		what:  "res." + method + "(" + n + ")",
		why:   "status code " + n + " is informational and not final (RFC 9110 §15.2)",
		fix:   "use res.WriteHeader(" + n + ") for an interim response, and a status from 200 to 599 here",
	})

	return StatusInternalServerError
}

// unlessSent reports that the header is not sent yet. After the send a change
// is lost (RFC 9110 §6.4.1): it fails and the caller ignores the change.
func (res *ResponseWriter) unlessSent(method, name string) bool {
	if !res.sent {
		return true
	}

	res.fail(strictError{
		level:  zap.WarnLevel,
		rule:   "change after the header was sent",
		what:   call(method, name),
		why:    "the header was already sent, so the change is lost (RFC 9110 §6.4.1)",
		fix:    "make the call before the first res.Send, res.String, res.JSON, res.Write or res.End",
		header: name,
	})

	return false
}

// noBody reports that no body was written yet. A response has one body: it
// fails and the caller ignores the second one.
func (res *ResponseWriter) noBody(method string) bool {
	if !res.bodySent {
		return true
	}

	res.fail(strictError{
		level: zap.WarnLevel,
		rule:  "second body",
		what:  "res." + method + "(...)",
		why:   "the response already sent a body, and a response has one body (RFC 9110 §6.4.1)",
		fix:   "send one body for each response: return after res.Send, res.String, res.JSON, res.Render or res.Error",
	})

	return false
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

// checkName reports that name is a field name (RFC 9110 §5.6.2). Else it fails
// and the caller ignores the call.
func (res *ResponseWriter) checkName(method, name string) bool {
	if validFieldName(name) {
		return true
	}

	why := "header name \"\" is not a token: it is empty (RFC 9110 §5.6.2)"

	if name != "" {
		i := firstInvalid(name, &tcharTable)
		why = "header name " + quoteValue(name) + " is not a token: it has " + byteName(name[i]) + " at position " + strconv.Itoa(i) + " (RFC 9110 §5.6.2)"
	}

	res.fail(strictError{
		level:  zap.ErrorLevel,
		rule:   "invalid header name",
		what:   call(method, name),
		why:    why,
		fix:    "use a name of letters, digits and !#$%&'*+-.^_`|~, for example \"X-Request-Id\"",
		header: name,
	})

	return false
}

// firstInvalid returns the index of the first byte that the table refuses.
func firstInvalid(s string, table *[256]bool) int {
	for i := 0; i < len(s); i++ {
		if !table[s[i]] {
			return i
		}
	}

	return 0
}

// byteName writes a byte for a message: '\n' (0x0a).
func byteName(b byte) string {
	const digits = "0123456789abcdef"

	hex := "0x" + string([]byte{digits[b>>4], digits[b&15]})

	if b < 0x80 {
		return strconv.QuoteRuneToASCII(rune(b)) + " (" + hex + ")"
	}

	return hex
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

// checkValue reports that value fits RFC 9110 §5.5. Else it fails and the
// caller ignores the call.
func (res *ResponseWriter) checkValue(method, name, value string) bool {
	if validFieldValue(value) {
		return true
	}

	i := firstInvalid(value, &valueTable)

	res.fail(strictError{
		level:  zap.ErrorLevel,
		rule:   "invalid header value",
		what:   call(method, name),
		why:    "header " + name + " has the value " + quoteValue(value) + ", which has the byte " + byteName(value[i]) + " at position " + strconv.Itoa(i) + " (RFC 9110 §5.5)",
		fix:    "remove the control bytes from the value; encode data from a user before it goes in a header",
		header: name,
		value:  value,
	})

	return false
}

// checkSpecial reports that name is not a field with its own method.
func (res *ResponseWriter) checkSpecial(method, name string) bool {
	switch {
	case equalASCIIFold(name, HeaderSetCookie):
		res.fail(strictError{
			level:  zap.WarnLevel,
			rule:   "Set-Cookie has its own method",
			what:   call(method, name),
			why:    "Set-Cookie can have many field lines and its value has its own rules (RFC 6265 §3)",
			fix:    "use res.Cookie(&http.Cookie{Name: \"sid\", Value: value})",
			header: name,
		})

		return false
	case equalASCIIFold(name, HeaderVary):
		fix := "use res.Vary(\"Accept\")"
		if method == "Own" {
			fix = "use res.OwnVary(\"Accept\")"
		}

		res.fail(strictError{
			level:  zap.WarnLevel,
			rule:   "Vary has its own method",
			what:   call(method, name),
			why:    "Vary is a list that only grows (RFC 9110 §12.5.5)",
			fix:    fix,
			header: name,
		})

		return false
	}

	return true
}

// Set replaces all values of the field name with value. These are mistakes:
// an invalid name or value, Set-Cookie and Vary (use Cookie and
// Vary), a call after the header was sent, and a field that a middleware owns
// (see Own). Development panics. Production logs and does not change the
// field. It does nothing after the client is gone.
func (res *ResponseWriter) Set(name, value string) {
	res.setField("Set", name, value)
}

func (res *ResponseWriter) setField(method, name, value string) {
	if !res.checkName(method, name) || !res.checkValue(method, name, value) || !res.checkSpecial(method, name) {
		return
	}

	if res.finished() {
		return
	}

	if !res.unlessSent(method, name) {
		return
	}

	if o := res.findOwned(name); o != nil {
		if value != o.value {
			res.failOverride(call(method, name), o, value)
		}

		return
	}

	res.w.Header().Set(name, value)
}

// Append adds members to the list field name. All members go in one field line
// and a member that is already there (same bytes) is skipped. Each element of
// values is one member: nothing is split. It does nothing after the client is
// gone.
//
// These are mistakes, as for Set: an empty member (Production skips
// it), a singleton field such as Content-Type (Production does a Set with the
// last value), an invalid name or value, Set-Cookie and Vary, a call after the
// header was sent, and a field that a middleware owns.
//
// Members are compared as whole strings. A value with a quoted comma, such as
// `rel="x, y"`, is not parsed.
func (res *ResponseWriter) Append(name string, values ...string) {
	if !res.checkName("Append", name) || !res.checkSpecial("Append", name) {
		return
	}

	if res.appendSingleton(name, values) || !res.checkMembers(name, values) {
		return
	}

	if res.finished() {
		return
	}

	if !res.unlessSent("Append", name) {
		return
	}

	if o := res.findOwned(name); o != nil {
		if appendNews(o.value, values) {
			res.failOverride(call("Append", name), o, strings.Join(values, ", "))
		}

		return
	}

	res.appendMembers(name, values)
}

// appendSingleton reports that name takes one value. It fails and does a Set
// with the last value, the same as a sequence of Set calls.
func (res *ResponseWriter) appendSingleton(name string, values []string) bool {
	for _, singleton := range singletons {
		if !equalASCIIFold(name, singleton.name) {
			continue
		}

		res.fail(strictError{
			level:  zap.WarnLevel,
			rule:   "Append on a single-value field",
			what:   call("Append", name),
			why:    singleton.name + " is a single-value field (" + singleton.basis + ")",
			fix:    "use res.Set(" + strconv.Quote(singleton.name) + ", value)",
			header: name,
		})

		if n := len(values); n > 0 {
			res.Set(name, values[n-1])
		}

		return true
	}

	return false
}

// checkMembers reports that each value is valid. An empty member fails but is
// not invalid: appendMembers skips it.
func (res *ResponseWriter) checkMembers(name string, values []string) bool {
	ok := true

	for _, value := range values {
		if !res.checkValue("Append", name, value) {
			ok = false
			continue
		}

		if isBlank(value) {
			res.fail(strictError{
				level:  zap.WarnLevel,
				rule:   "empty list member",
				what:   call("Append", name),
				why:    "header " + name + " has an empty member (RFC 9110 §5.6.1.1)",
				fix:    "pass only members with text, for example res.Append(" + strconv.Quote(name) + ", \"a\", \"b\")",
				header: name,
			})
		}
	}

	return ok
}

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

// appendMembers merges the members into the field line. It skips an empty
// member.
func (res *ResponseWriter) appendMembers(name string, values []string) {
	header := res.w.Header()
	lines := header.Values(name)
	line := joinLines(lines)
	multi := len(lines) > 1 // always leave one field line

	if line == "" && len(values) == 1 && !isBlank(values[0]) {
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

// isOldMember reports if values[i] is in the line, is a copy of an earlier
// value, or is empty (it is skipped).
func isOldMember(line string, values []string, i int) bool {
	return isBlank(values[i]) || listHas(line, values[i]) || slices.Contains(values[:i], values[i])
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

// Remove deletes the field name. The mistakes are the same as for Set; Vary
// has no Remove. It does nothing after the client is gone.
func (res *ResponseWriter) Remove(name string) {
	if !res.checkName("Remove", name) {
		return
	}

	if equalASCIIFold(name, HeaderVary) {
		res.fail(strictError{
			level:  zap.WarnLevel,
			rule:   "Vary has no Remove",
			what:   call("Remove", name),
			why:    "Vary is a list that only grows: a member can be added, not removed (RFC 9110 §12.5.5)",
			fix:    "use res.Vary(\"Accept\") to add a member",
			header: name,
		})

		return
	}

	if res.finished() {
		return
	}

	if !res.unlessSent("Remove", name) {
		return
	}

	if o := res.findOwned(name); o != nil {
		res.failOverride(call("Remove", name), o, "")
		return
	}

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
// and "*" replaces all. Vary stays one field line. An invalid value and a call
// after the header was sent are mistakes, as for Set. It does nothing after the
// client is gone. A handler can add members to a Vary that a middleware owns.
func (res *ResponseWriter) Vary(names ...string) {
	res.addVary("Vary", names)
}

func (res *ResponseWriter) addVary(method string, names []string) bool {
	for _, value := range names {
		if !res.checkValue(method, HeaderVary, value) {
			return false
		}
	}

	if res.finished() || !res.unlessSent(method, "") {
		return false
	}

	res.mergeVary(names)

	return true
}

// varyQuiet adds a Vary member for a method that reads the request (see
// Request.Accepts). It does nothing, and is not a mistake, after the send.
func (res *ResponseWriter) varyQuiet(name string) {
	if res.sent || res.finished() {
		return
	}

	res.mergeVary([]string{name})
}

func (res *ResponseWriter) mergeVary(names []string) {
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
// can have many lines. A nil or invalid cookie and a call after the header was
// sent are mistakes, as for Set: Production does not send the cookie. It does
// nothing after the client is gone.
func (res *ResponseWriter) Cookie(cookie *http.Cookie) {
	if !res.checkCookie(cookie) {
		return
	}

	if res.finished() {
		return
	}

	if !res.unlessSent("Cookie", HeaderSetCookie) {
		return
	}

	res.w.Header().Add(HeaderSetCookie, cookie.String())
}

func (res *ResponseWriter) checkCookie(cookie *http.Cookie) bool {
	if cookie == nil {
		res.fail(strictError{
			level:  zap.WarnLevel,
			rule:   "nil cookie",
			what:   "res.Cookie(nil)",
			why:    "the cookie is nil, so there is nothing to send (RFC 6265 §4.1)",
			fix:    "pass a cookie: res.Cookie(&http.Cookie{Name: \"sid\", Value: value})",
			header: HeaderSetCookie,
		})

		return false
	}

	err := cookie.Valid()
	if err == nil {
		return true
	}

	res.fail(strictError{
		level:  zap.WarnLevel,
		rule:   "invalid cookie",
		what:   "res.Cookie(" + quoteValue(cookie.Name) + ")",
		why:    "cookie " + quoteValue(cookie.Name) + " is invalid: " + err.Error() + " (RFC 6265 §4.1.1)",
		fix:    "use a cookie name that is a token and a value of cookie-octets; encode other data first",
		header: HeaderSetCookie,
	})

	return false
}

// ClearCookie asks the client to delete the cookie name (Path "/").
func (res *ResponseWriter) ClearCookie(name string) {
	// The cookie is deleted, so it needs no Secure, HttpOnly or SameSite.
	res.Cookie(&http.Cookie{Name: name, Path: "/", MaxAge: -1}) //nolint:gosec // a deletion cookie has no value to protect
}

// Location sets the Location field. See Set.
func (res *ResponseWriter) Location(value string) {
	res.setField("Location", HeaderLocation, value)
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

// SendStatus sends the status line and no body. A code outside 200-599 and a
// call after the header was sent are mistakes, as for Status.
func (res *ResponseWriter) SendStatus(code int) {
	code = res.checkFinalStatus("SendStatus", code)

	if res.finished() {
		return
	}

	if !res.unlessSent("SendStatus", "") {
		return
	}

	res.code = code
	res.writeHeader(code)
}

func (res *ResponseWriter) Send(v []byte) {
	if res.finished() {
		return
	}

	if !res.noBody("Send") {
		return
	}

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

	if !res.noBody("Render") {
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

	if !res.noBody("Error") {
		return
	}

	if res.code == 0 {
		res.code = StatusInternalServerError
	}

	payload := []byte(e.Error())

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(payload)))
	res.defaultContentType(MIMETextPlainCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(payload)
}

func (res *ResponseWriter) Redirect(value string) {
	if res.finished() {
		return
	}

	if !res.noBody("Redirect") {
		return
	}

	// A redirect without Location is useless: Production answers 500.
	if !res.checkValue("Redirect", HeaderLocation, value) {
		res.SendStatus(StatusInternalServerError)
		return
	}

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

	if !res.noBody("String") {
		return
	}

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

	if !res.noBody("JSON") {
		return
	}

	payload, err := json.Marshal(v)

	if err != nil {
		panic(err)
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(payload)))
	res.defaultContentType(MIMEApplicationJSONCharsetUTF8)

	res.writeHeader(res.code)
	res.bodySent = true
	_, _ = res.w.Write(payload)
}

// SendFile serves the file at the path name. It serves the path that the caller
// gives, so absolute paths and symbolic links are served. A path with a ".."
// element gets 404. Do not pass request data without validation.
//
// A status set before SendFile other than 200 is the status of the response:
// the file is the body, and Range and the conditional fields are not used.
// Otherwise SendFile answers them (RFC 9110 sections 13 and 14), and an
// If-Match or If-None-Match with an invalid syntax gets 400.
func (res *ResponseWriter) SendFile(name string) {
	if res.finished() {
		return
	}

	if !res.noBody("SendFile") {
		return
	}

	if hasDotDot(name) {
		res.logger().Error("path traversal refused", zap.String("filename", name))
		res.SendStatus(StatusNotFound)
		return
	}

	code := res.code

	if code == 0 {
		code = StatusOK
		res.code = code
	}

	if code == StatusOK && !etag.ValidRequest(res.request.Header) {
		res.SendStatus(StatusBadRequest)
		return
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

	if code != StatusOK {
		res.sendFileWithStatus(code, file, file_name, file_stat)
		return
	}

	hash := crc32.ChecksumIEEE([]byte(file_name + file_modtime.String() + strconv.FormatInt(file_size, 10)))
	res.Header().Set(HeaderEtag, fmt.Sprintf(`W/"%d-%08x"`, file_size, hash))

	// http.ServeContent answers the conditional and range fields (RFC 9110
	// section 13 and 14): If-Match, If-None-Match, If-Modified-Since,
	// If-Unmodified-Since, If-Range and Range with one or more ranges.
	w := http.ResponseWriter(res)
	req, none := fileRequest(res.request)

	if none {
		res.Header().Set(HeaderContentRange, "bytes */"+strconv.FormatInt(file_size, 10))
		res.SendStatus(StatusRequestedRangeNotSatisfiable)
		return
	}

	if req.Header.Get(HeaderRange) != "" {
		w = rangeWriter{ResponseWriter: res, size: file_size}
	}

	http.ServeContent(w, req, file_name, file_modtime, file)
}

// sendFileWithStatus sends the file as the body of a response with the status
// code that the handler set. Range and the conditional fields apply to a 2xx
// response only (RFC 9110 sections 13.2.1 and 14.2).
func (res *ResponseWriter) sendFileWithStatus(code int, file *os.File, name string, info os.FileInfo) {
	ctype := mime.TypeByExtension(filepath.Ext(name))

	if ctype == "" {
		var buf [512]byte
		n, _ := io.ReadFull(file, buf[:])
		ctype = http.DetectContentType(buf[:n])

		if _, err := file.Seek(0, io.SeekStart); err != nil {
			res.logger().Error("seeker can't seek", zap.Error(err))
			res.SendStatus(StatusInternalServerError)
			return
		}
	}

	h := res.Header()
	h.Set(HeaderLastModified, info.ModTime().UTC().Format(http.TimeFormat))

	// A 204 and a 304 have no content (RFC 9110 sections 15.3.5, 15.4.5).
	noContent := code == StatusNoContent || code == StatusNotModified

	if !noContent {
		h.Set(HeaderContentType, ctype)
		h.Set(HeaderContentLength, strconv.FormatInt(info.Size(), 10))
	}

	res.writeHeader(code)

	if noContent || res.request.Method == MethodHead {
		return
	}

	res.bodySent = true
	_, _ = file.WriteTo(res.w)
}

// headerIfRange is not public: only SendFile reads it.
const headerIfRange = "If-Range"

// rangeWriter adds "Content-Range: bytes */size" to each 416 of
// http.ServeContent, also for a Range with an invalid syntax (RFC 9110
// section 15.5.17).
type rangeWriter struct {
	*ResponseWriter
	size int64
}

func (w rangeWriter) WriteHeader(code int) {
	if h := w.Header(); code == StatusRequestedRangeNotSatisfiable && h.Get(HeaderContentRange) == "" {
		h.Set(HeaderContentRange, "bytes */"+strconv.FormatInt(w.size, 10))
	}

	w.ResponseWriter.WriteHeader(code)
}

// fileRequest returns the request that http.ServeContent gets. Range is
// defined for GET only, so any other method loses Range and If-Range (RFC 9110
// section 14.2). A Range with a unit that is not "bytes" is ignored (section
// 14.2), and the unit is not case-sensitive (section 14.1). A suffix of no
// byte is dropped from the set (section 14.1.2). none is true when the set has
// no member left: it is not satisfiable, and the caller answers 416 (sections
// 14.1.1 and 14.1.2). Any other value stays, and ServeContent answers 416 when
// the syntax is invalid.
func fileRequest(req *http.Request) (_ *http.Request, none bool) {
	rng := req.Header.Get(HeaderRange)

	if rng == "" {
		return req, false
	}

	if req.Method != MethodGet {
		return fileHeaderEdit(req, ""), false
	}

	unit, set, ok := strings.Cut(rng, "=")

	if !ok || unit == "" || !isToken(unit) {
		return req, false
	}

	if !strings.EqualFold(unit, "bytes") {
		return fileHeaderEdit(req, ""), false
	}

	set, none = fileRangeSet(set)

	if none {
		return req, true
	}

	if fixed := "bytes=" + set; fixed != rng {
		return fileHeaderEdit(req, fixed), false
	}

	return req, false
}

// fileHeaderEdit returns a copy of req with the Range value set, or without
// Range and If-Range when value is empty.
func fileHeaderEdit(req *http.Request, value string) *http.Request {
	req = req.WithContext(req.Context())
	req.Header = req.Header.Clone()

	if value == "" {
		req.Header.Del(HeaderRange)
		req.Header.Del(headerIfRange)

		return req
	}

	req.Header.Set(HeaderRange, value)

	return req
}

// fileRangeSet drops the members of a bytes range set that have no byte: the
// empty ones and the suffix of length 0 (RFC 9110 section 14.1.2). none is
// true when no member is left, or a member has a sign.
func fileRangeSet(set string) (_ string, none bool) {
	kept, drop := 0, false

	for rest := set; rest != ""; {
		var m string
		m, rest, _ = strings.Cut(rest, ",")

		if fileRangeBad(m) {
			return "", true
		}

		if fileRangeHasByte(m) {
			kept++
		} else if strings.TrimSpace(m) != "" {
			drop = true
		}
	}

	switch {
	case kept == 0:
		return "", true
	case !drop:
		return set, false
	}

	var b strings.Builder

	for rest := set; rest != ""; {
		var m string
		m, rest, _ = strings.Cut(rest, ",")

		if !fileRangeHasByte(m) {
			continue
		}

		if b.Len() > 0 {
			b.WriteByte(',')
		}

		b.WriteString(m)
	}

	return b.String(), false
}

// fileRangeBad reports that the member m has a sign. A number is digits only
// (RFC 9110 section 14.1.2), and ParseInt in ServeContent accepts a sign.
func fileRangeBad(m string) bool {
	_, last, _ := strings.Cut(m, "-")

	return strings.Contains(m, "+") || strings.HasPrefix(strings.TrimSpace(last), "-")
}

// fileRangeHasByte reports that the member m is not empty and is not a suffix
// of length 0.
func fileRangeHasByte(m string) bool {
	m = strings.TrimSpace(m)

	if m == "" {
		return false
	}

	first, last, ok := strings.Cut(m, "-")

	if !ok || strings.TrimSpace(first) != "" {
		return true
	}

	n, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)

	return err != nil || n != 0
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
