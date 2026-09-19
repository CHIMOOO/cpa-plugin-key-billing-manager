package turnstate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyCheckUsesUnauthenticatedAPIAndFreshTraceConnection(t *testing.T) {
	var connections []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections = append(connections, r.RemoteAddr)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("connectivity check sent an account or proxy credential to the endpoint")
		}
		if !r.Close || r.ProtoMajor != 1 {
			t.Error("connectivity check reused a persistent connection")
		}
		if r.URL.Path == "/api" {
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || string(body) != "{}" || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("unexpected API check request: method=%s body=%s", r.Method, body)
			}
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			if r.Method != http.MethodGet {
				t.Error("trace did not use GET")
			}
			_, _ = io.WriteString(w, "fl=123\nip=203.0.113.42\ncolo=XXX\n")
		}
	}))
	defer server.Close()
	for range 2 {
		result := doProxyCheck(probeTestClient(t, "", server.Certificate()), server.URL+"/api", server.URL+"/trace", "http://dummy:dummy-password@proxy.example:80", nil)
		if result.Status != "reachable" || result.IP != "203.0.113.42" || result.HTTPStatus != 401 || result.Deletable || result.Proxy != "http://***@proxy.example:80" {
			t.Fatalf("unexpected connectivity result: %+v", result)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "dummy-password") {
			t.Fatal("test response leaked proxy credentials")
		}
	}
	seen := map[string]bool{}
	for _, connection := range connections {
		if seen[connection] {
			t.Fatal("connectivity and trace requests reused a connection")
		}
		seen[connection] = true
	}
	if len(seen) != 4 {
		t.Fatalf("connections = %d, want 4", len(seen))
	}
}

func TestProxyCheckDoesNotTreatThrottlingOrTraceFailuresAsDeadProxy(t *testing.T) {
	for _, status := range []int{302, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api" {
					t.Error("inconclusive API response triggered trace")
				}
				w.Header().Set(Header, strings.Repeat("x", 312))
				w.WriteHeader(status)
			}))
			defer server.Close()
			result := doProxyCheck(probeTestClient(t, ""), server.URL+"/api", server.URL+"/trace", "http://proxy.example:80", nil)
			if result.Status != "inconclusive" || result.Deletable || result.HTTPStatus != status {
				t.Fatalf("remote HTTP %d was treated as a dead proxy: %+v", status, result)
			}
		})
	}
	for _, body := range []string{"ip=not-an-ip", "ip=203.0.113.1,203.0.113.2", strings.Repeat("x", proxyCheckBodyLimit+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		result := doProxyCheck(probeTestClient(t, ""), server.URL+"/api", server.URL+"/trace", "http://proxy.example:80", nil)
		server.Close()
		if result.Status != "reachable" || result.IP != "" || result.Deletable {
			t.Fatalf("bad trace changed a reachable result: %+v", result)
		}
	}
}

func TestProxyCheckProxyAuthenticationAndRefusedConnection(t *testing.T) {
	for _, status := range []int{407, 403, 429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected proxy request: %+v", r)
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "dummy-password must never be returned")
			}))
			defer proxy.Close()
			result, err := CheckProxy(strings.Replace(proxy.URL, "://", "://dummy:dummy-password@", 1))
			if err != nil || result.Deletable != (status == 407) || result.HTTPStatus != status {
				t.Fatalf("proxy status result=%+v, err=%v", result, err)
			}
			if status == 407 && result.Status != "failed" || status != 407 && result.Status != "inconclusive" {
				t.Fatalf("proxy status classification=%+v", result)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "dummy-password") {
				t.Fatal("proxy diagnostic exposed response body or URL credential")
			}
		})
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	result, err := CheckProxy("http://dummy:dummy-password@" + address)
	if err != nil || result.Status != "failed" || !result.Deletable {
		t.Fatalf("refused proxy connection not eligible for removal: result=%+v err=%v", result, err)
	}
}

func TestProxyCheckTimeoutAndResponseCleanup(t *testing.T) {
	for _, stage := range []string{"api", "trace"} {
		t.Run(stage, func(t *testing.T) {
			closed := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/api" && stage == "trace" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				<-r.Context().Done()
				closed <- struct{}{}
			}))
			defer server.Close()
			client := probeTestClient(t, "")
			client.Timeout = 50 * time.Millisecond
			started := time.Now()
			result := doProxyCheck(client, server.URL+"/api", server.URL+"/trace", "http://proxy.example:80", nil)
			if time.Since(started) > time.Second || result.Deletable || (stage == "api" && result.Status != "inconclusive") || (stage == "trace" && result.Status != "reachable") {
				t.Fatalf("incorrect timeout result: %+v", result)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("connectivity check left a timed-out connection active")
			}
		})
	}
}

func TestProxyCheckRejectsMalformedURLsWithoutEchoingSecrets(t *testing.T) {
	for _, proxy := range []string{"", "direct", "ftp://dummy:dummy-password@host:21", "http://dummy:dummy-password@host:80/path", "http://dummy:dummy-password@host:80?key=x", "http://dummy:dummy-password@host:80\r\nheader: x", "http://host:65536", "http://" + strings.Repeat("x", 4096)} {
		result, err := CheckProxy(proxy)
		encoded, _ := json.Marshal(result)
		if err != nil || result.Status != "failed" || !result.Deletable || strings.Contains(string(encoded), "dummy-password") {
			t.Fatalf("invalid proxy accepted or credentials reflected: %q, %+v, %v", proxy, result, err)
		}
	}
}

func TestProxyCheckSOCKSAuthenticationFailure(t *testing.T) {
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
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				done <- rejectSOCKSAuthentication(connection)
			}()
			result, err := CheckProxy(scheme + "://dummy:dummy-password@" + listener.Addr().String())
			if err != nil || result.Status != "failed" || !result.Deletable || result.Reason != "Proxy authentication failed" {
				t.Errorf("SOCKS auth result=%+v err=%v", result, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func rejectSOCKSAuthentication(connection net.Conn) error {
	reader := bufio.NewReader(connection)
	var prefix [2]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, reader, int64(prefix[1])); err != nil {
		return err
	}
	if _, err := connection.Write([]byte{5, 2}); err != nil {
		return err
	}
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, reader, int64(prefix[1])); err != nil {
		return err
	}
	length, err := reader.ReadByte()
	if err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
		return err
	}
	_, err = connection.Write([]byte{1, 1})
	return err
}

func TestProxyCheckCONNECTCallbackCanFinishAfterCancellation(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	client, transport, err := newProbeClient(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client.Timeout = 50 * time.Millisecond
	var status atomic.Int32
	callbackStarted, releaseCallback, callbackFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		close(callbackStarted)
		<-releaseCallback
		status.Store(int32(response.StatusCode))
		close(callbackFinished)
		return nil
	}
	completed := make(chan ProxyCheckResult, 1)
	go func() { completed <- doProxyCheck(client, probeEndpoint, proxyTraceEndpoint, proxy.URL, &status) }()
	<-callbackStarted
	select {
	case result := <-completed:
		if result.Deletable || result.Status != "inconclusive" || result.HTTPStatus != 0 {
			t.Fatalf("canceled check incorrectly used an unfinished callback: %+v", result)
		}
	case <-time.After(time.Second):
		close(releaseCallback)
		t.Fatal("canceled check waited for the CONNECT callback")
	}
	// The callback can still be running even though the management call already
	// returned. Exercise concurrent snapshots as it publishes the late result.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_ = status.Load()
			select {
			case <-callbackFinished:
				return
			default:
			}
		}
	}()
	close(releaseCallback)
	<-callbackFinished
	<-readerDone
	if status.Load() != http.StatusProxyAuthRequired {
		t.Fatal("late CONNECT status was not published")
	}
}
