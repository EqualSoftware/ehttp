package ehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EqualSoftware/ehttp/internal/ehttptest"
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

			ehttptest.Eq(t, "design 2.2: X-Forwarded-* only from a peer in a trusted range", got, want)
		})
	}
}

func TestTrustedProxiesInvalidPanics(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"", "bogus", "10.0.0.1/33", "10.0.0.1/", "1.2.3", "true", "*"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()

			defer func() {
				ehttptest.True(t, "design 2.2: an invalid value panics at boot", recover() != nil, "no panic for %q", v)
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
		{"default trusts nobody", "design 2.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https", "X-Forwarded-Host", "x.com"}},
		{"untrusted peer", "design 2.2", "http|example.com|example.com", "1.1.1.1:1", []string{"X-Forwarded-Proto", "https", "X-Forwarded-Host", "x.com"}},
		{"proto https", "design 2.2", "https|example.com|example.com", peer, []string{"X-Forwarded-Proto", "HTTPS"}},
		{"protocol header", "design 2.2", "https|example.com|example.com", peer, []string{"X-Forwarded-Protocol", "https"}},
		{"proto http", "design 2.2", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "http"}},
		{"proto list first", "design 2.2 (Express)", "https|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https, http"}},
		{"proto other value ignored", "author premise: strict", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "javascript"}},
		{"proto scheme form ignored", "author premise: strict", "http|example.com|example.com", peer, []string{"X-Forwarded-Proto", "https://"}},
		{"host list first", "design 2.2 (Express)", "http|a.com|a.com", peer, []string{"X-Forwarded-Host", "a.com, b.com"}},
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
	ehttptest.Eq(t, "S8 row: default ReadHeaderTimeout is 10 s", def.ReadHeaderTimeout, 10*time.Second)
	ehttptest.Eq(t, "S8 row: other options keep the stdlib default", def.ReadTimeout, time.Duration(0))
	ehttptest.Eq(t, "S8 row: other options keep the stdlib default", def.MaxHeaderBytes, 0)

	s := NewServer(
		WithReadHeaderTimeout(1*time.Second), WithReadTimeout(2*time.Second), WithWriteTimeout(3*time.Second),
		WithIdleTimeout(4*time.Second), WithMaxHeaderBytes(5), WithShutdownTimeout(6*time.Second),
	)
	h := s.httpServer()

	ehttptest.Eq(t, "S8 row", h.ReadHeaderTimeout, 1*time.Second)
	ehttptest.Eq(t, "S8 row", h.ReadTimeout, 2*time.Second)
	ehttptest.Eq(t, "S8 row", h.WriteTimeout, 3*time.Second)
	ehttptest.Eq(t, "S8 row", h.IdleTimeout, 4*time.Second)
	ehttptest.Eq(t, "S8 row", h.MaxHeaderBytes, 5)
	ehttptest.Eq(t, "S8 row", s.shutdownTimeout, 6*time.Second)
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
				ehttptest.True(t, "author premise (2026-11-01): invalid boot input panics", recover() != nil, "no panic")
			}()

			f()
		})
	}
}

// S8 row: the shutdown has its own deadline. At the deadline it closes the
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

	ehttptest.Eq(t, "S8 row: the deadline error is returned", got, context.DeadlineExceeded)
	ehttptest.True(t, "S8 row: Close ends the connection of the stuck request", <-clientErr != nil, "client got a response")
}
