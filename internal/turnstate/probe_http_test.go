package turnstate

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func probeTestClient(t *testing.T, proxy string, certificates ...*x509.Certificate) *http.Client {
	t.Helper()
	client, transport, err := newProbeClient(proxy)
	if err != nil {
		t.Fatal(err)
	}
	if len(certificates) > 0 {
		transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		for _, certificate := range certificates {
			transport.TLSClientConfig.RootCAs.AddCert(certificate)
		}
	}
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

func TestHTTPProbeRequestAndSSECleanup(t *testing.T) {
	requests := make(chan *http.Request, 2)
	bodies := make(chan map[string]any, 2)
	closed := make(chan struct{}, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- r.Clone(r.Context())
		bodies <- body
		w.Header().Set(Header, "upstream-state")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer server.Close()
	// Direct means direct even if the process has ambient proxy settings.
	t.Setenv("HTTPS_PROXY", "http://unreachable.invalid:1")
	t.Setenv("HTTP_PROXY", "http://unreachable.invalid:1")
	t.Setenv("NO_PROXY", "")
	credential, _ := dummyCredential("")
	var previous *http.Request
	for range 2 {
		client := probeTestClient(t, "", server.Certificate())
		start := time.Now()
		result, err := doHTTPProbe(client, server.URL, credential, "model-a")
		if err != nil || result.Status != 200 || result.Value != "upstream-state" {
			t.Fatalf("probe = %+v, %v", result, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("probe waited for the SSE body instead of returning its headers")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("probe left its SSE connection running after return")
		}
		request, body := <-requests, <-bodies
		if request.Method != http.MethodPost || request.ProtoMajor != 1 || !request.Close || request.Header.Get("Authorization") != "Bearer dummy-oauth-token" || request.Header.Get("Chatgpt-Account-Id") != "dummy-account-id" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("Originator") != "codex-tui" || request.Header.Get("User-Agent") != "codex-tui/0.154.0 (Linux; x86_64)" {
			t.Fatalf("unexpected upstream request: %+v", request)
		}
		if len(request.Header.Get("Session-Id")) != 32 || (previous != nil && (request.RemoteAddr == previous.RemoteAddr || request.Header.Get("Session-Id") == previous.Header.Get("Session-Id"))) {
			t.Fatal("probe reused a connection or session")
		}
		previous = request
		if body["model"] != "model-a" || body["stream"] != true || body["store"] != false || body["instructions"] != "" || body["parallel_tool_calls"] != false || body["reasoning"].(map[string]any)["effort"] != "low" || !strings.Contains(fmt.Sprint(body["input"]), "Reply OK.") {
			t.Fatalf("probe payload changed: %+v", body)
		}
	}
}

func TestHTTPProbeHonorsFinalStatusesWithoutRedirecting(t *testing.T) {
	for _, status := range []int{200, 302, 401, 403, 429, 502} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(Header, "final-upstream-state")
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
			}))
			defer server.Close()
			credential, _ := dummyCredential("")
			result, err := doHTTPProbe(probeTestClient(t, ""), server.URL, credential, "model-a")
			if err != nil || result.Status != status || result.Value != "final-upstream-state" || redirected.Load() != 0 {
				t.Fatalf("final response = %+v, %v; redirected=%d", result, err, redirected.Load())
			}
		})
	}
}

func TestHTTPProbeTimeoutTLSAndHeaderLimits(t *testing.T) {
	credential, _ := dummyCredential("")
	t.Run("timeout-closes-connection", func(t *testing.T) {
		closed := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			close(closed)
		}))
		defer server.Close()
		client := probeTestClient(t, "")
		client.Timeout = 100 * time.Millisecond
		start := time.Now()
		if result, err := doHTTPProbe(client, server.URL, credential, "model-a"); err == nil || result.Status != 0 {
			t.Fatalf("timeout = %+v, %v", result, err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("request exceeded its total timeout")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("timed out probe left a connection running")
		}
	})
	t.Run("untrusted-tls", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("untrusted TLS endpoint received OAuth credential")
		}))
		defer server.Close()
		if result, err := doHTTPProbe(probeTestClient(t, ""), server.URL, credential, "model-a"); err == nil || result.Status != 0 {
			t.Fatalf("untrusted TLS = %+v, %v", result, err)
		}
	})
	t.Run("oversized-headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set(Header, strings.Repeat("x", 128<<10)) }))
		defer server.Close()
		if result, err := doHTTPProbe(probeTestClient(t, ""), server.URL, credential, "model-a"); err == nil || result.Status != 0 {
			t.Fatalf("oversized headers = %+v, %v", result, err)
		}
	})
}

func TestHTTPAndHTTPSProbeProxies(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", secure), func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to upstream")
				}
				w.Header().Set(Header, "real-upstream-state")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer upstream.Close()
			connections := make(chan string, 2)
			proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("dummy-user:dummy:p@ssword="))
				if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(upstream.URL, "https://") || r.Header.Get("Proxy-Authorization") != wantAuth || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected proxy request: %+v", r)
					w.WriteHeader(http.StatusProxyAuthRequired)
					return
				}
				connections <- r.RemoteAddr
				target, err := net.Dial("tcp", r.Host)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				defer target.Close()
				connection, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer connection.Close()
				fmt.Fprintf(buffered, "HTTP/1.1 200 Connection established\r\n%s: misleading-proxy-state\r\n\r\n", Header)
				if err = buffered.Flush(); err != nil {
					t.Error(err)
					return
				}
				tunnelProbeTest(connection, buffered, target)
			})
			proxy := httptest.NewUnstartedServer(proxyHandler)
			if secure {
				proxy.StartTLS()
			} else {
				proxy.Start()
			}
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			proxyURL.User = url.UserPassword("dummy-user", "dummy:p@ssword=")
			certificates := []*x509.Certificate{upstream.Certificate()}
			if secure {
				certificates = append(certificates, proxy.Certificate())
			}
			credential, _ := dummyCredential("")
			for range 2 {
				result, err := doHTTPProbe(probeTestClient(t, proxyURL.String(), certificates...), upstream.URL, credential, "model-a")
				if err != nil || result.Status != 429 || result.Value != "real-upstream-state" {
					t.Fatalf("proxy response confused with upstream: %+v, %v", result, err)
				}
			}
			if first, second := <-connections, <-connections; first == second {
				t.Fatal("rotating proxy attempts reused a connection")
			}
		})
	}
}

func tunnelProbeTest(client net.Conn, reader io.Reader, target net.Conn) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(target, reader)
		_ = target.Close()
		close(done)
	}()
	_, _ = io.Copy(client, target)
	_ = client.Close()
	<-done
}

func TestSOCKSProbeAuthenticationAndRemoteDNS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set(Header, "socks-upstream-state") }))
	defer upstream.Close()
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
				err = serveProbeSOCKS(connection, strings.TrimPrefix(upstream.URL, "http://"))
				done <- err
			}()
			proxy := scheme + "://dummy-user:dummy-password@" + listener.Addr().String()
			// This domain cannot resolve locally. The SOCKS server must receive
			// its name unchanged, then connect to our local fixture itself.
			endpoint := strings.Replace(upstream.URL, "127.0.0.1", "probe.invalid", 1)
			credential, _ := dummyCredential("")
			result, err := doHTTPProbe(probeTestClient(t, proxy), endpoint, credential, "model-a")
			if err != nil || result.Status != 200 || result.Value != "socks-upstream-state" {
				t.Fatalf("SOCKS probe = %+v, %v", result, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func serveProbeSOCKS(connection net.Conn, targetAddress string) error {
	reader := bufio.NewReader(connection)
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		return err
	}
	methods := make([]byte, int(prefix[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	if prefix[0] != 5 || !strings.ContainsRune(string(methods), 2) {
		return fmt.Errorf("SOCKS5 username/password negotiation missing")
	}
	if _, err := connection.Write([]byte{5, 2}); err != nil {
		return err
	}
	if _, err := io.ReadFull(reader, prefix); err != nil {
		return err
	}
	username := make([]byte, int(prefix[1]))
	if _, err := io.ReadFull(reader, username); err != nil {
		return err
	}
	length, err := reader.ReadByte()
	if err != nil {
		return err
	}
	password := make([]byte, int(length))
	if _, err := io.ReadFull(reader, password); err != nil {
		return err
	}
	if prefix[0] != 1 || string(username) != "dummy-user" || string(password) != "dummy-password" {
		return fmt.Errorf("SOCKS authentication did not preserve credentials")
	}
	if _, err := connection.Write([]byte{1, 0}); err != nil {
		return err
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != 5 || header[1] != 1 || header[3] != 3 {
		return fmt.Errorf("SOCKS destination was not a remote domain")
	}
	address := make([]byte, int(header[4])+2)
	if _, err := io.ReadFull(reader, address); err != nil {
		return err
	}
	_, expectedPort, _ := net.SplitHostPort(targetAddress)
	if string(address[:len(address)-2]) != "probe.invalid" || strconv.Itoa(int(binary.BigEndian.Uint16(address[len(address)-2:]))) != expectedPort {
		return fmt.Errorf("SOCKS target mismatch")
	}
	target, err := net.Dial("tcp", targetAddress)
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err := connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return err
	}
	tunnelProbeTest(connection, reader, target)
	return nil
}

func TestHTTPProbeHarvestKeepsAccountScope(t *testing.T) {
	m, now := newTestManager(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set(Header, tokenAt(*now)) }))
	defer server.Close()
	m.runProbe = func(credential Credential, model, proxy string) (ProbeResponse, error) {
		return doHTTPProbe(probeTestClient(t, proxy, server.Certificate()), server.URL, credential, model)
	}
	result, err := m.Probe("account-a", "model-a", dummyCredential)
	if err != nil || result.Action != "harvested" {
		t.Fatalf("HTTP harvest = %+v, %v", result, err)
	}
	rows := m.Status().Templates
	if len(rows) != 1 || rows[0].Account != "account-a" || rows[0].Model != "model-a" {
		t.Fatalf("HTTP harvest was not isolated to selected account/model: %+v", rows)
	}
}

func TestHTTPProbeClientLimits(t *testing.T) {
	client := probeTestClient(t, "")
	transport := client.Transport.(*http.Transport)
	if client.Timeout != 25*time.Second || transport.TLSHandshakeTimeout != 10*time.Second || transport.ResponseHeaderTimeout != 25*time.Second || !transport.DisableKeepAlives || transport.MaxResponseHeaderBytes != 64<<10 || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("probe transport lost its security or lifetime bounds")
	}
	for _, proxy := range []string{"file:///tmp/example", "socks4://proxy.invalid:1080", "://bad"} {
		if _, _, err := newProbeClient(proxy); err == nil {
			t.Fatalf("unsupported proxy accepted: %q", proxy)
		}
	}
}
