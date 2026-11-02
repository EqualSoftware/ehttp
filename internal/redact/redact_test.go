package redact

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func TestSensitive(t *testing.T) {
	t.Parallel()

	extra, err := Default().With("x_internal", "*-pin", "tenant*", "*card*")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		m    *Matcher
		name string
		want bool
	}{
		{nil, "password", true},
		{nil, "User-Password-Hash", true},  // contains
		{nil, "X-Api-Key", true},           // ends with, "-" is "_"
		{nil, "api_key", true},             // ends with
		{nil, "APIKEY", true},              // case
		{nil, "monkey", true},              // ends with key
		{nil, "keyword", false},            // key is not at the end
		{nil, "keyboard", false},           // key is not at the end
		{nil, "sig", true},                 // exact
		{nil, "Sig", true},                 // exact, case
		{nil, "signal", false},             // exact only
		{nil, "AWSAccessKeyId", true},      // exact
		{nil, "awsaccesskeyid", true},      // exact, case
		{nil, "Authorization", true},       // contains auth
		{nil, "Set-Cookie", true},          // contains cookie
		{nil, "X_CSRF_Token", true},        // case and "_"
		{nil, "page", false},               // plain
		{nil, "", false},                   // empty name
		{extra, "x-internal", true},        // added exact, "-" is "_"
		{extra, "x-internal-id", false},    // exact only
		{extra, "user-pin", true},          // added suffix
		{extra, "tenant-id", true},         // added prefix
		{extra, "my-tenant", false},        // prefix only
		{extra, "creditcard-number", true}, // added contains
		{nil, "x-internal", false},         // the default is not changed
		{Default(), "tenant-id", false},    // With does not change its receiver
	}

	for _, tt := range tests {
		if got := tt.m.Sensitive(tt.name); got != tt.want {
			t.Errorf("Sensitive(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestWithInvalid(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"", "*", "**", "***", "a*b", "*a*b*", "**a", "a**", "a b", "a,b", "é", "*a\x00"} {
		if _, err := Default().With(p); err == nil {
			t.Errorf("With(%q) has no error", p)
		}
	}

	for _, p := range []string{"a", "*a", "a*", "*a*", "X-My_Key.1", "a~b"} {
		if _, err := Default().With(p); err != nil {
			t.Errorf("With(%q) = %v", p, err)
		}
	}
}

func TestValue(t *testing.T) {
	t.Parallel()

	var m *Matcher

	if got := m.Value("Authorization", "Bearer abc"); got != Mask {
		t.Errorf("got %q", got)
	}

	if got := m.Value("Accept", "text/html"); got != "text/html" {
		t.Errorf("got %q", got)
	}
}

func TestHeader(t *testing.T) {
	t.Parallel()

	in := http.Header{
		"Authorization": {"Bearer a"},
		"Set-Cookie":    {"a=1", "b=2"},
		"Accept":        {"text/html"},
	}
	want := http.Header{
		"Authorization": {Mask},
		"Set-Cookie":    {Mask, Mask},
		"Accept":        {"text/html"},
	}

	got := (*Matcher)(nil).Header(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}

	if in.Get("Authorization") != "Bearer a" || in["Set-Cookie"][1] != "b=2" {
		t.Errorf("input changed: %v", in)
	}

	if (*Matcher)(nil).Header(nil) != nil {
		t.Error("nil header")
	}
}

func TestURL(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"https://host/p?api_key=abc&page=2", "https://host/p?api_key=***&page=2"},
		{"https://host/p?page=2&token=a&token=b&x=1", "https://host/p?page=2&token=***&token=***&x=1"},
		{"https://u:pw@host/p", "https://***:***@host/p"},
		{"https://u@host/p", "https://***@host/p"},
		{"https://u:@host/p", "https://***:***@host/p"},
		{"https://host/a/b#frag", "https://host/a/b"},
		{"https://host/p?a=1&b=2", "https://host/p?a=1&b=2"},
		{"https://host/p?TOKEN=a", "https://host/p?TOKEN=***"},
		{"https://host/p?api%5Fkey=a&api+key=b", "https://host/p?api%5Fkey=***&api+key=***"},
		{"https://host/p?access%2Dtoken=a", "https://host/p?access%2Dtoken=***"},
		{"https://host/p?token&page=1", "https://host/p?token&page=1"},
		{"https://host/p?token=", "https://host/p?token=***"},
		{"https://host/p?a=1&&token=x&", "https://host/p?a=1&&token=***&"},
		{"https://host/p?x=1;api_key=S&b=2", "https://host/p?x=1;api_key=***&b=2"},
		{"https://host/p?password=a;b&x=1", "https://host/p?password=***&x=1"},
		{"https://host/p?tok%zzen=V&a=1", "https://host/p?tok%zzen=***&a=1"},
		{"https://host/p?=S&a=1", "https://host/p?=***&a=1"},
		{"http:user:pass@host/x?token=S&a=1", "http:***?token=***&a=1"},
		{"https://host/p?password=a b&x=1", "https://host/p?password=***&x=1"},
	}

	for _, tt := range tests {
		u, err := url.Parse(tt.in)
		if err != nil {
			// "a b" is not a valid raw query for Parse; keep the test URL as built.
			u = &url.URL{Scheme: "https", Host: "host", Path: "/p", RawQuery: "password=a b&x=1"}
		}

		before := *u

		if got := (*Matcher)(nil).URL(u); got != tt.want {
			t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
		}

		if !reflect.DeepEqual(before, *u) {
			t.Errorf("URL(%q) changed the input", tt.in)
		}
	}

	if (*Matcher)(nil).URL(nil) != "" {
		t.Error("nil url")
	}
}

func TestAllocs(t *testing.T) {
	m := Default()
	u, _ := url.Parse("https://host/p?a=1&page=2")

	if n := testing.AllocsPerRun(100, func() { m.Sensitive("Content-Type") }); n != 0 {
		t.Errorf("Sensitive allocs = %v", n)
	}

	if n := testing.AllocsPerRun(100, func() { m.Value("Authorization", "x") }); n != 0 {
		t.Errorf("Value allocs = %v", n)
	}

	if n := testing.AllocsPerRun(100, func() { m.Query(u.RawQuery) }); n != 0 {
		t.Errorf("Query allocs = %v", n)
	}
}

func BenchmarkSensitiveMiss(b *testing.B) {
	m := Default()
	b.ReportAllocs()

	for b.Loop() {
		m.Sensitive("Content-Type")
	}
}

func BenchmarkSensitiveHit(b *testing.B) {
	m := Default()
	b.ReportAllocs()

	for b.Loop() {
		m.Sensitive("Authorization")
	}
}

func BenchmarkQueryMiss(b *testing.B) {
	m := Default()
	b.ReportAllocs()

	for b.Loop() {
		m.Query("a=1&page=2&sort=name")
	}
}

func BenchmarkURL(b *testing.B) {
	m := Default()
	u, _ := url.Parse("https://u:p@host/p?api_key=abc&page=2")
	b.ReportAllocs()

	for b.Loop() {
		m.URL(u)
	}
}
