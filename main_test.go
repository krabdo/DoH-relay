package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testConfig(raw string) config {
	u, _ := url.Parse(raw)
	return config{upstream: u, path: "/dns-query", timeout: time.Second}
}

func TestWirePassthrough(t *testing.T) {
	// example.com A query, EDNS OPT containing ECS 192.0.2.0/24.
	wire, _ := hex.DecodeString("123401000001000000000001076578616d706c6503636f6d000001000100002904d000000000000b0008000700011800c00002")
	for _, h2 := range []bool{false, true} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(method+map[bool]string{false: "/h1", true: "/h2"}[h2], func(t *testing.T) {
				var calls atomic.Int32
				var connections atomic.Int32
				up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.ProtoMajor != map[bool]int{false: 1, true: 2}[h2] {
						t.Errorf("protocol: %s", r.Proto)
					}
					if r.Method != method || r.URL.EscapedPath() != "/custom%2Fendpoint" {
						t.Errorf("method/path: %s %s", r.Method, r.URL)
					}
					if r.URL.RawQuery != "token=a%2fb&dns=AA_-&x=%2f&x=%2F&extra=a+b" {
						t.Errorf("query: %s", r.URL.RawQuery)
					}
					if r.Header.Get("X-Test") != "untouched" || r.Header.Get("X-Hop") != "" {
						t.Error("headers changed")
					}
					if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Accept-Encoding") != "" {
						t.Error("invented headers")
					}
					body, _ := io.ReadAll(r.Body)
					if method == "POST" && !bytes.Equal(body, wire) {
						t.Error("DNS/ECS body changed")
					}
					w.Header().Set("Content-Type", "application/dns-message")
					w.Header().Set("Content-Encoding", "gzip") // Opaque bytes must not be decoded.
					w.Header().Set("X-Reply", "preserved")
					w.WriteHeader(418)
					_, _ = w.Write(wire)
				}))
				up.EnableHTTP2 = h2
				up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
					if state == http.StateNew {
						connections.Add(1)
					}
				}
				up.StartTLS()
				defer up.Close()
				tr := newTransport()
				tr.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				defer tr.CloseIdleConnections()
				proxy := handler(testConfig(up.URL+"/custom%2Fendpoint?token=a%2fb"), tr)
				for range 2 {
					var body io.Reader
					if method == "POST" {
						body = bytes.NewReader(wire)
					}
					r := httptest.NewRequest(method, "http://relay/dns-query?dns=AA_-&x=%2f&x=%2F&extra=a+b", body)
					r.Header.Set("X-Test", "untouched")
					r.Header.Set("Content-Type", "application/dns-message")
					r.Header.Set("Connection", "X-Hop")
					r.Header.Set("X-Hop", "remove")
					w := httptest.NewRecorder()
					proxy.ServeHTTP(w, r)
					if w.Code != 418 || !bytes.Equal(w.Body.Bytes(), wire) || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("X-Reply") != "preserved" {
						t.Errorf("response changed: %v", w)
					}
				}
				if calls.Load() != 2 {
					t.Fatal("unexpected retries")
				}
				if connections.Load() != 1 {
					t.Fatal("upstream connection was not reused")
				}
			})
		}
	}
}

func TestForwardedAndRawQuery(t *testing.T) {
	h := handler(testConfig("https://upstream.test/dns"), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Host != "upstream.test" || r.URL.RawQuery != "x=%zz;a&x=2" {
			t.Error("host/query changed")
		}
		if r.Header.Get("Forwarded") != "for=192.0.2.1" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("forwarded headers")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://elsewhere.test/"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	}))
	r := httptest.NewRequest("GET", "/dns-query?x=%zz;a&x=2", nil)
	r.Header.Set("Forwarded", "for=192.0.2.1")
	r.Header.Set("X-Forwarded-For", "remove")
	r.Header.Set("Connection", "X-Forwarded-For")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 302 || w.Body.String() != "redirect" {
		t.Fatal("redirect followed")
	}
}

func TestErrorsAndNoLogs(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	for _, tc := range []struct {
		name      string
		status    int
		transport roundTripFunc
	}{
		{"failure", 502, func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive query") }},
		{"timeout", 504, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig("https://upstream.test/dns")
			c.timeout = 10 * time.Millisecond
			w := httptest.NewRecorder()
			handler(c, tc.transport).ServeHTTP(w, httptest.NewRequest("GET", "/dns-query", nil))
			if w.Code != tc.status || strings.Contains(w.Body.String(), "sensitive") {
				t.Fatal(w)
			}
		})
	}
	if logs.Len() != 0 {
		t.Fatalf("request logged: %s", &logs)
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	h := handler(testConfig("https://upstream.test/dns"), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/dns-query", nil).WithContext(ctx))
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not propagate")
	}
}

func TestRouting(t *testing.T) {
	h := handler(testConfig("https://upstream.test"), roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected upstream request"); return nil, nil }))
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/other", 404}, {"DELETE", "/dns-query", 405}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code {
			t.Fatal(w.Code)
		}
	}
}

func TestConfig(t *testing.T) {
	t.Setenv("UPSTREAM_URL", "https://example.com:8443/dns-query?token=a")
	t.Setenv("REQUEST_TIMEOUT", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("DOH_PATH", "")
	c, err := readConfig()
	if err != nil || c.listen != ":8080" || c.timeout != 10*time.Second {
		t.Fatal(c, err)
	}
	for _, raw := range []string{"", "http://example.com", "https://user:secret@example.com", "https://example.com/#fragment"} {
		t.Setenv("UPSTREAM_URL", raw)
		if _, err := readConfig(); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	t.Setenv("UPSTREAM_URL", "https://example.com")
	t.Setenv("REQUEST_TIMEOUT", "0s")
	if _, err := readConfig(); err == nil {
		t.Fatal("zero timeout")
	}
	t.Setenv("REQUEST_TIMEOUT", "10s")
	t.Setenv("DOH_PATH", "relative")
	if _, err := readConfig(); err == nil {
		t.Fatal("relative path")
	}
}

func BenchmarkRelay(b *testing.B) {
	body := strings.Repeat("x", 64)
	h := handler(testConfig("https://upstream.test/dns-query"), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	b.ReportAllocs()
	b.SetBytes(64)
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/dns-query", strings.NewReader(body)))
	}
}
