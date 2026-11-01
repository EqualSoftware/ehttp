package ehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"html/template"
	"io"
	"mime"
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

func (i *ResponseWriterInterceptor) WriteHeader(code int) {
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

	i.ResponseWriter.WriteHeader(i.Code)
	i.ResponseWriter.Write(i.Buffer.Bytes())
}

// create a new ResponseWriter with a Intercept as Writer
// this help to capture the value and apply some process,
// like etag, and proceed to dispatch the response
func WithResponseWriter(res *ResponseWriter) *ResponseWriter {
	return &ResponseWriter{
		Writer: &ResponseWriterInterceptor{
			ResponseWriter: res.Writer,
			Buffer:         &bytes.Buffer{},
			Code:           res.code,
		},
		request:  res.request,
		code:     res.code,
		template: res.template,
	}
}

type ResponseWriter struct {
	Writer   http.ResponseWriter
	request  *http.Request
	code     int
	template *template.Template
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

	return res.Writer.Header()
}

func (res *ResponseWriter) StatusCode() int {
	return res.code
}

func (res *ResponseWriter) Status(code int) {
	if res.finished() {
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

	vary := res.Header().Get(HeaderVary)

	if vary == "" {
		vary = value
	} else if !strings.Contains(strings.ToLower(vary), strings.ToLower(value)) {
		vary = strings.ReplaceAll(vary, ", ", ",")

		parts := strings.Split(vary, ",")
		parts = append(parts, value)

		vary = strings.Join(parts, ", ")
	}

	res.Header().Set(HeaderVary, vary)
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

	res.Writer.WriteHeader(res.code)
}

func (res *ResponseWriter) SendStatus(code int) {
	if res.finished() {
		return
	}

	res.code = code

	res.Writer.WriteHeader(code)
}

func (res *ResponseWriter) Send(v []byte) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	res.Header().Set(HeaderContentLength, strconv.Itoa(len(v)))

	res.Writer.WriteHeader(res.code)
	res.Writer.Write(v)
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

	res.Writer.WriteHeader(res.code)
	buf.WriteTo(res.Writer)
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

	res.Writer.WriteHeader(res.code)
	res.Writer.Write(bytes)
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

	res.Writer.WriteHeader(res.code)
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

	res.Writer.WriteHeader(res.code)
	res.Writer.Write(b)
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

	res.Writer.WriteHeader(res.code)
	res.Writer.Write(bytes)
}

func (res *ResponseWriter) SendFile(name string) {
	if res.finished() {
		return
	}

	if res.code == 0 {
		res.code = StatusOK
	}

	file, err := os.Open(name)

	if err != nil {
		zap.L().Error("file not found", zap.String("filename", name))
		res.SendStatus(StatusNotFound)
		return
	}

	defer file.Close()

	file_stat, err := file.Stat()

	if err != nil || file_stat.IsDir() {
		zap.L().Error("invalid file", zap.String("filename", name))
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
				zap.L().Error("seeker can't seek", zap.Error(err))
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

		res.Writer.WriteHeader(res.code)
		file.Seek(start, io.SeekStart)
		io.CopyN(res.Writer, file, end-start+1)
		return
	}

	res.Header().Set(HeaderContentLength, strconv.FormatInt(file_size, 10))

	res.Writer.WriteHeader(res.code)
	file.WriteTo(res.Writer)
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
