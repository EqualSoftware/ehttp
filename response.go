package ehttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Response struct {
	*http.Response
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
// means stale.
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

// Read the response.Body as JSON. It reads the JSON value into memory.
func (res *Response) JSON(value interface{}) error {
	defer res.Body.Close()

	err := json.NewDecoder(res.Body).Decode(value)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return err
	}

	// Read the rest of a small body so the connection can be reused. A bigger
	// body closes the connection (same limit as net/http maxPostHandlerReadBytes).
	_, _ = io.CopyN(io.Discard, res.Body, 256<<10)

	return nil
}

// Read the response.Body as String. It reads the full body into memory.
func (res *Response) String() (string, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return "", err
	}

	return string(bytes), nil
}

// Read the response.Body as Bytes. It reads the full body into memory.
func (res *Response) Bytes() ([]byte, error) {
	defer res.Body.Close()

	bytes, err := io.ReadAll(res.Body)

	if err != nil {
		zap.L().Error("failure to parse the json response", zap.Error(err))

		return nil, err
	}

	return bytes, nil
}
