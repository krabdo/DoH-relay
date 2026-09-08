package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	upstream     *url.URL
	listen, path string
	timeout      time.Duration
}

func readConfig() (config, error) {
	c := config{listen: env("LISTEN_ADDR", ":8080"), path: env("DOH_PATH", "/dns-query")}
	u, err := url.Parse(os.Getenv("UPSTREAM_URL"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return c, errors.New("UPSTREAM_URL must be an HTTPS URL without userinfo or fragment")
	}
	c.upstream = u
	if !strings.HasPrefix(c.path, "/") || strings.ContainsAny(c.path, "?#") {
		return c, errors.New("DOH_PATH must be an absolute URL path without query or fragment")
	}
	c.timeout, err = time.ParseDuration(env("REQUEST_TIMEOUT", "10s"))
	if err != nil || c.timeout <= 0 {
		return c, errors.New("REQUEST_TIMEOUT must be a positive duration")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ForceAttemptHTTP2 = true
	t.DisableCompression = true
	t.MaxIdleConns = 128
	t.MaxIdleConnsPerHost = 128
	return t
}

type copyBuffers struct{ pool sync.Pool }

func (b *copyBuffers) Get() []byte {
	if v := b.pool.Get(); v != nil {
		return *v.(*[]byte)
	}
	return make([]byte, 32<<10)
}

func (b *copyBuffers) Put(buf []byte) { b.pool.Put(&buf) }

func handler(c config, transport http.RoundTripper) http.Handler {
	p := &httputil.ReverseProxy{
		BufferPool: &copyBuffers{},
		Transport:  transport,
		ErrorLog:   log.New(io.Discard, "", 0),
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = c.upstream.Scheme
			p.Out.URL.Host = c.upstream.Host
			p.Out.URL.Path = c.upstream.Path
			p.Out.URL.RawPath = c.upstream.RawPath
			p.Out.URL.RawQuery = c.upstream.RawQuery
			if p.In.URL.RawQuery != "" {
				if p.Out.URL.RawQuery != "" {
					p.Out.URL.RawQuery += "&"
				}
				p.Out.URL.RawQuery += p.In.URL.RawQuery
			}
			p.Out.URL.ForceQuery = c.upstream.ForceQuery || p.In.URL.ForceQuery
			p.Out.Host = c.upstream.Host
			// Rewrite removes forwarding headers. Restore only supplied end-to-end
			// values, never invent client identity or restore Connection tokens.
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if values, ok := p.In.Header[name]; ok && !connectionToken(p.In.Header, name) {
					p.Out.Header[name] = append([]string(nil), values...)
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			http.Error(w, http.StatusText(status), status)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != c.path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), c.timeout)
		defer cancel()
		p.ServeHTTP(w, r.WithContext(ctx))
	})
}

func connectionToken(h http.Header, name string) bool {
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), name) {
				return true
			}
		}
	}
	return false
}

func main() {
	c, err := readConfig()
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
	t := newTransport()
	defer t.CloseIdleConnections()
	s := &http.Server{Addr: c.listen, Handler: handler(c, t), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: c.timeout, WriteTimeout: c.timeout + time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.Shutdown(shutdown) != nil {
			_ = s.Close()
		}
	}()
	log.Print("DoH relay starting")
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print("HTTP listener failed")
		os.Exit(1)
	}
	stop()
	<-done
	log.Print("DoH relay stopped")
}
