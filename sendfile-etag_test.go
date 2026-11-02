package ehttp

import (
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
)

var strongETag = regexp.MustCompile(`^"[\x21\x23-\x7e\x80-\xff]*"$`)

func TestFileETag(t *testing.T) {
	t.Parallel()

	instant := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	brt := time.FixedZone("BRT", -3*3600)
	base := fileETag(instant, 10)

	tests := []struct {
		name, basis string
		got         string
		same        bool
	}{
		{"same instant in another zone", "RFC 9110 §8.8.1: a strong tag changes only when the representation changes, not with the zone", fileETag(instant.In(brt), 10), true},
		{"mtime differs by 1 ns", "RFC 9110 §8.8.1: a strong validator changes with each representation", fileETag(instant.Add(time.Nanosecond), 10), false},
		{"size differs", "RFC 9110 §8.8.1: a strong validator changes with each representation", fileETag(instant, 11), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ehttptest.Eq(t, tt.basis, tt.got == base, tt.same)
		})
	}

	t.Run("time before 1970 has no sign", func(t *testing.T) {
		t.Parallel()

		tag := fileETag(time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC), 10)
		ehttptest.True(t, "RFC 9110 §8.8.3: the tag stays a valid entity-tag", strongETag.MatchString(tag), "tag %s", tag)
		ehttptest.True(t, "RFC 9110 §8.8.3", tag[1] != '-', "tag %s", tag)
	})
}

// SendFile sends a strong entity tag, and the conditional fields work with it.
func TestSendFileETag(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	tag := dSend(s, http.MethodGet, "/f").Header().Get(HeaderEtag)

	t.Run("header is a strong entity-tag", func(t *testing.T) {
		t.Parallel()
		ehttptest.True(t, "RFC 9110 §8.8.3: DQUOTE *etagc DQUOTE, no W/", strongETag.MatchString(tag), "tag %s", tag)
	})

	tests := []struct {
		name, basis string
		kv          []string
		code        int
		body        string
	}{
		{"If-None-Match with the tag", "RFC 9110 §13.1.2: weak comparison", []string{HeaderIfNoneMatch, tag}, 304, ""},
		{"If-Match with the tag", "RFC 9110 §13.1.1: strong comparison", []string{"If-Match", tag}, 200, dRangeBody},
		{"If-Match with another tag", "RFC 9110 §13.1.1: strong comparison", []string{"If-Match", `"other"`}, 412, ""},
		{"If-Range with the tag", "RFC 9110 §13.1.5", []string{HeaderRange, "bytes=0-9", "If-Range", tag}, 206, dRangeBody},
		{"If-Range with another tag", "RFC 9110 §13.1.5: the Range is ignored", []string{HeaderRange, "bytes=0-9", "If-Range", `"other"`}, 200, dRangeBody},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := dSend(s, http.MethodGet, "/f", tt.kv...)
			ehttptest.Eq(t, tt.basis, rec.Code, tt.code)
			ehttptest.Eq(t, tt.basis, rec.Body.String(), tt.body)
		})
	}
}

// A rewrite with a new mtime gives a new tag (RFC 9110 §8.8.1).
func TestSendFileETagChangesOnRewrite(t *testing.T) {
	t.Parallel()

	name := dRangeFile(t)
	s := NewServer()
	s.GET("/f", func(res *ResponseWriter, _ *Request) { res.SendFile(name) })

	first := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(name, first, first); err != nil {
		t.Fatal(err)
	}

	before := dSend(s, http.MethodGet, "/f").Header().Get(HeaderEtag)

	if err := os.WriteFile(name, []byte("abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := first.Add(time.Hour)
	if err := os.Chtimes(name, second, second); err != nil {
		t.Fatal(err)
	}

	after := dSend(s, http.MethodGet, "/f").Header().Get(HeaderEtag)

	ehttptest.True(t, "RFC 9110 §8.8.1", before != "" && before != after, "before %s, after %s", before, after)
}

// HEAD gives the header fields that GET gives (RFC 9110 §9.3.2).
func TestSendFileETagHeadEqualsGet(t *testing.T) {
	t.Parallel()

	s := dRangeServer(t)
	get := dSend(s, http.MethodGet, "/f").Header().Get(HeaderEtag)
	head := dSend(s, http.MethodHead, "/f").Header().Get(HeaderEtag)

	ehttptest.True(t, "RFC 9110 §9.3.2", get != "" && get == head, "get %s, head %s", get, head)
}
