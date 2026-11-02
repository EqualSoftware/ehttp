package ehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
	"go.uber.org/zap"
)

// aWho serves GET /x and answers "protocol|host|hostname".
func aWho(trusted ...string) *Server {
	var s *Server
	if trusted == nil {
		s = NewServer()
	} else {
		s = NewServer(WithTrustedProxies(trusted...))
	}

	s.GET("/x", func(res *ResponseWriter, req *Request) {
		res.String(fmt.Sprintf("%s|%s|%s", req.Protocol(), req.Host(), req.Hostname()))
	})

	return s
}

func aWhoDo(s *Server, remote string, kv ...string) string {
	r := httptest.NewRequestWithContext(context.Background(), "GET", "/x", http.NoBody)
	r.RemoteAddr = remote

	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	return w.Body.String()
}

func TestTrustedProxyPeers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		trust, remote string
		want          bool
	}{
		{"loopback", "127.0.0.1:1", true},
		{"loopback", "127.5.5.5:1", true},
		{"loopback", "[::1]:1", true},
		{"loopback", "[::ffff:127.0.0.1]:1", true},
		{"loopback", "128.0.0.1:1", false},
		{"loopback", "garbage", false},
		{"loopback", "", false},
		{"LOOPBACK", "127.0.0.1:1", true},
		{"linklocal", "169.254.1.1:1", true},
		{"linklocal", "[fe80::1%eth0]:80", true},
		{"linklocal", "[fec0::1]:1", false},
		{"uniquelocal", "10.1.1.1:1", true},
		{"uniquelocal", "172.31.0.1:1", true},
		{"uniquelocal", "172.32.0.1:1", false},
		{"uniquelocal", "192.168.1.1:1", true},
		{"uniquelocal", "[fd00::1]:1", true},
		{"uniquelocal", "[fe00::1]:1", false},
		{"10.0.0.0/8", "10.9.9.9:1", true},
		{"10.0.0.1/8", "10.9.9.9:1", true},
		{"10.0.0.1", "10.0.0.1:1", true},
		{"10.0.0.1", "10.0.0.2:1", false},
		{"::ffff:10.0.0.0/104", "10.0.0.1:1", true},
		{"::ffff:10.0.0.0/104", "11.0.0.1:1", false},
		{"0.0.0.0/0", "1.2.3.4:1", true},
	}

	for _, tt := range tests {
		t.Run(tt.trust+" "+tt.remote, func(t *testing.T) {
			t.Parallel()

			got := aWhoDo(aWho(tt.trust), tt.remote, "X-Forwarded-Proto", "https")
			want := "http|example.com|example.com"

			if tt.want {
				want = "https|example.com|example.com"
			}

			ehttptest.Eq(t, "X-Forwarded-* counts only from a peer in a trusted range", got, want)
		})
	}
}

func TestTrustedProxiesInvalidPanics(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"", "bogus", "10.0.0.1/33", "10.0.0.1/", "1.2.3", "true", "*"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()

			defer func() {
				ehttptest.True(t, "an invalid value panics at boot", recover() != nil, "no panic for %q", v)
			}()

			WithTrustedProxies(v)
		})
	}
}

func TestForwardedHeaders(t *testing.T) {
	t.Parallel()

	const peer = "192.0.2.1:1"

	tests := []struct {
		name, basis, want string
		peer              string
		kv                []string
	}{
		{"default trusts nobody", "X-Forwarded-* counts only from a trusted peer", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https", "X-Forwarded-Host", "x.com"}},
		{"untrusted peer", "X-Forwarded-* counts only from a trusted peer", "http|example.com|example.com", "1.1.1.1:1", []string{"X-Forwarded-Proto", "https", "X-Forwarded-Host", "x.com"}},
		{"proto https", "X-Forwarded-* counts only from a trusted peer", "https|example.com|example.com", peer, []string{"X-Forwarded-Proto", "HTTPS"}},
		{"protocol header", "X-Forwarded-* counts only from a trusted peer", "https|example.com|example.com", peer, []string{"X-Forwarded-Protocol", "https"}},
		{"proto http", "X-Forwarded-* counts only from a trusted peer", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "http"}},
		{"proto list first", "Express: the first list member wins", "https|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https, http"}},
		{"proto other value ignored", "only http and https are valid schemes; other values are ignored", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "javascript"}},
		{"proto scheme form ignored", "only http and https are valid schemes; other values are ignored", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https://"}},
		{"host list first", "Express: the first list member wins", "http|a.com|a.com", peer, []string{"X-Forwarded-Host", "a.com, b.com"}},
		{"host with port", "RFC 9110 §7.2", "http|a.com:8443|a.com", peer, []string{"X-Forwarded-Host", "a.com:8443"}},
		{"host ipv6 with port", "RFC 3986 §3.2.2", "http|[::1]:8080|::1", peer, []string{"X-Forwarded-Host", "[::1]:8080"}},
		{"host upper case", "RFC 3986 §3.2.2", "http|A.COM|a.com", peer, []string{"X-Forwarded-Host", "A.COM"}},
		{"host empty first element", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", ", b.com"}},
		{"host with path", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "evil.com/x?y=1#z"}},
		{"host with userinfo", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "a@evil.com"}},
		{"host with space", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "a b"}},
		{"host bare ipv6", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "::1"}},
		{"host open bracket", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "[::1"}},
		{"host empty port", "RFC 9110 §7.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Host", "a.com:"}},
	}

	s := aWho("192.0.2.1")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := s
			if tt.name == "default trusts nobody" {
				srv = aWho()
			}

			ehttptest.Eq(t, tt.basis, aWhoDo(srv, tt.peer, tt.kv...), tt.want)
		})
	}
}

func TestServerOptionsReachHTTPServer(t *testing.T) {
	t.Parallel()

	def := NewServer().httpServer()
	ehttptest.Eq(t, "default ReadHeaderTimeout is 10 s", def.ReadHeaderTimeout, 10*time.Second)
	ehttptest.Eq(t, "other options keep the stdlib default", def.ReadTimeout, time.Duration(0))
	ehttptest.Eq(t, "other options keep the stdlib default", def.MaxHeaderBytes, 0)

	s := NewServer(
		WithReadHeaderTimeout(1*time.Second), WithReadTimeout(2*time.Second), WithWriteTimeout(3*time.Second),
		WithIdleTimeout(4*time.Second), WithMaxHeaderBytes(5), WithShutdownTimeout(6*time.Second),
	)
	h := s.httpServer()

	ehttptest.Eq(t, "the option reaches http.Server", h.ReadHeaderTimeout, 1*time.Second)
	ehttptest.Eq(t, "the option reaches http.Server", h.ReadTimeout, 2*time.Second)
	ehttptest.Eq(t, "the option reaches http.Server", h.WriteTimeout, 3*time.Second)
	ehttptest.Eq(t, "the option reaches http.Server", h.IdleTimeout, 4*time.Second)
	ehttptest.Eq(t, "the option reaches http.Server", h.MaxHeaderBytes, 5)
	ehttptest.Eq(t, "the option reaches http.Server", s.shutdownTimeout, 6*time.Second)
}

func TestServerNegativeOptionsPanic(t *testing.T) {
	t.Parallel()

	tests := map[string]func(){
		"ReadHeaderTimeout": func() { WithReadHeaderTimeout(-1) },
		"ReadTimeout":       func() { WithReadTimeout(-1) },
		"WriteTimeout":      func() { WithWriteTimeout(-1) },
		"IdleTimeout":       func() { WithIdleTimeout(-1) },
		"MaxHeaderBytes":    func() { WithMaxHeaderBytes(-1) },
		"ShutdownTimeout":   func() { WithShutdownTimeout(-1) },
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				ehttptest.True(t, "an invalid boot input panics", recover() != nil, "no panic")
			}()

			f()
		})
	}
}

// The shutdown has its own deadline. At the deadline it closes the
// connections and returns the error.
func TestServerShutdownDeadlineClosesAndReturnsError(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})

	s := NewServer(WithShutdownTimeout(50 * time.Millisecond))
	s.GET("/hang", func(res *ResponseWriter, req *Request) {
		close(started)
		<-release
	})

	server := s.httpServer()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	serveErr := make(chan error, 1)

	go func() { serveErr <- server.Serve(ln) }()

	clientErr := make(chan error, 1)

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/hang", http.NoBody)

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		clientErr <- err
	}()

	<-started

	got := s.shutdown(context.Background(), server, serveErr)
	close(release)

	ehttptest.Eq(t, "the deadline error is returned", got, context.DeadlineExceeded)
	ehttptest.True(t, "Close ends the connection of the stuck request", <-clientErr != nil, "client got a response")
}

// A trusted proxy that sends an invalid value is a configuration mistake that
// the operator must see: the value is ignored and one warning names it.
func TestInvalidForwardedHeaderWarns(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, header, value string }{
		{"proto word", "X-Forwarded-Proto", "javascript"},
		{"proto scheme form", "X-Forwarded-Proto", "https://"},
		{"host with path", "X-Forwarded-Host", "evil.com/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l, logs := aObserved()
			s := NewServer(WithLogger(l), WithTrustedProxies("192.0.2.1"))
			s.GET("/x", func(res *ResponseWriter, req *Request) {
				res.String(req.Protocol() + "|" + req.Host() + "|" + req.Hostname())
			})

			ehttptest.Eq(t, "the connection value is used", aWhoDo(s, "192.0.2.1:1", tt.header, tt.value), "http|example.com|example.com")

			entries := logs.FilterLevelExact(zap.WarnLevel).All()
			ehttptest.Eq(t, "one warning for each request", len(entries), 1)

			if len(entries) != 1 {
				return
			}

			f := entries[0].ContextMap()
			ehttptest.Eq(t, "the header is named", fmt.Sprint(f["header"]), tt.header)
			ehttptest.Eq(t, "the value is quoted", fmt.Sprint(f["value"]), strconv.Quote(tt.value))
			ehttptest.Eq(t, "the peer is named", fmt.Sprint(f["peer"]), "192.0.2.1:1")
			ehttptest.True(t, "the log has a fix", f["fix"] != "", "no fix field")
		})
	}

	t.Run("a long value is truncated", func(t *testing.T) {
		t.Parallel()

		l, logs := aObserved()
		s := NewServer(WithLogger(l), WithTrustedProxies("192.0.2.1"))
		s.GET("/x", func(res *ResponseWriter, req *Request) { res.String(req.Protocol()) })

		aWhoDo(s, "192.0.2.1:1", "X-Forwarded-Proto", strings.Repeat("a", 500))

		v, _ := logs.FilterLevelExact(zap.WarnLevel).All()[0].ContextMap()["value"].(string)
		ehttptest.True(t, "the log keeps a bounded value", len(v) < 100, "value has %d bytes", len(v))
	})

	t.Run("an untrusted peer does not warn", func(t *testing.T) {
		t.Parallel()

		l, logs := aObserved()
		s := NewServer(WithLogger(l), WithTrustedProxies("192.0.2.1"))
		s.GET("/x", func(res *ResponseWriter, req *Request) { res.String(req.Protocol()) })

		aWhoDo(s, "198.51.100.1:1", "X-Forwarded-Proto", "javascript")

		ehttptest.Eq(t, "only a trusted proxy is told about", logs.FilterLevelExact(zap.WarnLevel).Len(), 0)
	})
}

// Protocol and Host can run in two goroutines of one request. The warning
// still comes one time, and the race detector sees no data race.
func TestInvalidForwardedHeaderWarnsOnceConcurrently(t *testing.T) {
	t.Parallel()

	l, logs := aObserved()
	s := NewServer(WithLogger(l), WithTrustedProxies("192.0.2.1"))
	s.GET("/x", func(res *ResponseWriter, req *Request) {
		var wg sync.WaitGroup

		wg.Add(2)

		go func() { defer wg.Done(); _ = req.Protocol() }()
		go func() { defer wg.Done(); _ = req.Host() }()

		wg.Wait()
		res.String("ok")
	})

	aWhoDo(s, "192.0.2.1:1", "X-Forwarded-Proto", "javascript", "X-Forwarded-Host", "evil.com/x")

	ehttptest.Eq(t, "one warning for each request", logs.FilterLevelExact(zap.WarnLevel).Len(), 1)
}
