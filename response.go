package ehttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Response struct {
	*http.Response

	maxBytes int64 // from WithMaxResponseBytes; 0 is no limit
}

// ErrResponseTooLarge is returned by Response.JSON, String and Bytes when the
// body is larger than the limit of WithMaxResponseBytes.
var ErrResponseTooLarge = errors.New("response body too large")

func (res *Response) tooLarge(where string) error {
	return fmt.Errorf("ehttp: %w in %s: the body is over the limit of %d bytes. "+
		"Fix: raise the limit with WithMaxResponseBytes, or use 0 for no limit", ErrResponseTooLarge, where, res.maxBytes)
}

// reader gives the body for a helper. With a limit, a known Content-Length
// above it is an error, and the read stops at one byte over it.
func (res *Response) reader(where string) (io.Reader, error) {
	if res.maxBytes == 0 {
		return res.Body, nil
	}

	// A HEAD response keeps the Content-Length of the resource with no body.
	head := res.Request != nil && res.Request.Method == http.MethodHead

	if !head && res.ContentLength > res.maxBytes {
		return nil, res.tooLarge(where)
	}

	return &capReader{r: io.LimitReader(res.Body, res.maxBytes+1), limit: res.maxBytes, err: res.tooLarge(where)}, nil
}

// capReader fails when more than limit bytes come from r.
type capReader struct {
	r     io.Reader
	n     int64
	limit int64
	err   error
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	if c.n > c.limit {
		return 0, c.err
	}

	return n, err
}

// readAll reads the body for String and Bytes.
func (res *Response) readAll(where string) ([]byte, error) {
	defer res.Body.Close()

	body, err := res.reader(where)
	if err == nil {
		var data []byte

		if data, err = io.ReadAll(body); err == nil {
			return data, nil
		}
	}

	zap.L().Error("failure to read the response body", zap.Error(err))

	return nil, err
}

// maxAgeCap is 2^31 seconds: the value for a max-age that is too large
// (RFC 9111 section 1.2.2).
const maxAgeCap = 1 << 31

// Retrieve the response.Header Cache-Control max-age as time.Duration. All
// the Cache-Control lines are one list (RFC 9110 section 5.3). The directive
// name is not case-sensitive, and the value can be a quoted-string (RFC 9111
// section 5.2). The value is delta-seconds: digits only (RFC 9111 section
// 1.2.2), and a value above 2^31 is 2^31. The first max-age directive wins
// (RFC 9111 section 4.2.1): if its value is invalid the result is zero, which
// means stale. A quote that does not end stops the scan of that line only: the
// next line is still read (an invalid directive is ignored).
func (res *Response) GetCacheControlMaxAge() time.Duration {
	for _, line := range res.Header.Values("Cache-Control") {
		for rest, more := line, true; more; {
			var directive string

			var ok bool

			directive, rest, more, ok = cutUnquoted(rest, ',')

			if !ok {
				break
			}

			name, value, found := strings.Cut(strings.TrimSpace(directive), "=")

			if !found || !strings.EqualFold(strings.TrimSpace(name), "max-age") {
				continue
			}

			return maxAgeValue(strings.TrimSpace(value))
		}
	}

	return 0
}

// maxAgeValue converts the value of max-age. An invalid value gives zero.
func maxAgeValue(value string) time.Duration {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}

	age, err := strconv.ParseUint(value, 10, 64)

	if errors.Is(err, strconv.ErrRange) {
		age = maxAgeCap // all digits, too large
	} else if err != nil {
		return 0
	}

	return time.Duration(min(age, maxAgeCap)) * time.Second
}

// Read the response.Body as JSON. It reads the JSON value into memory. The
// limit of WithMaxResponseBytes applies.
func (res *Response) JSON(value interface{}) error {
	defer res.Body.Close()

	body, err := res.reader("Response.JSON")
	if err == nil {
		err = json.NewDecoder(body).Decode(value)
	}

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return err
	}

	// Read the rest of a small body so the connection can be reused. A bigger
	// body closes the connection (same limit as net/http maxPostHandlerReadBytes).
	_, _ = io.CopyN(io.Discard, res.Body, 256<<10)

	return nil
}

// Read the response.Body as String. It reads the full body into memory. The
// limit of WithMaxResponseBytes applies.
func (res *Response) String() (string, error) {
	data, err := res.readAll("Response.String")
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// Read the response.Body as Bytes. It reads the full body into memory. The
// limit of WithMaxResponseBytes applies.
func (res *Response) Bytes() ([]byte, error) {
	return res.readAll("Response.Bytes")
}
