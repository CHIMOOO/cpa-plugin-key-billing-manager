package turnstate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyCheckOnlyConnectsGatewayWithoutSendingData(t *testing.T) {
	// Environment proxies must not redirect the direct gateway connection.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			wantIP := "127.0.0.1"
			if network == "tcp6" {
				address, wantIP = "[::1]:0", "::1"
			}
			for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
				t.Run(scheme, func(t *testing.T) {
					listener, err := net.Listen(network, address)
					if err != nil {
						if network == "tcp6" {
							t.Skipf("IPv6 unavailable: %v", err)
						}
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
						_ = connection.SetDeadline(time.Now().Add(time.Second))
						var payload [1]byte
						n, err := connection.Read(payload[:])
						if n != 0 || err != io.EOF {
							done <- fmt.Errorf("gateway received protocol data or connection was not closed: n=%d err=%v", n, err)
							return
						}
						done <- nil
					}()
					proxy := scheme + "://dummy:dummy-password@" + listener.Addr().String()
					result, err := CheckProxy("  " + proxy + "  ")
					if err != nil || result.Status != "reachable" || result.GatewayIP != wantIP || result.Deletable || result.IP != "" || result.HTTPStatus != 0 {
						t.Fatalf("unexpected TCP connectivity result: %+v, %v", result, err)
					}
					encoded, _ := json.Marshal(result)
					if strings.Contains(string(encoded), "dummy-password") || strings.Contains(string(encoded), `"ip":`) || strings.Contains(string(encoded), `"http_status":`) {
						t.Fatal("response leaked credentials or represented the gateway as an exit/API response")
					}
					if result.Proxy != scheme+"://***@"+listener.Addr().String() || result.ReasonMessage.Text != result.Reason {
						t.Fatalf("TCP result lost masking or translation metadata: %+v", result)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("gateway connection was not released before check returned")
					}
				})
			}
		})
	}
}

func TestProxyCheckGatewayAddressAndNoCredentialsPassedToDialer(t *testing.T) {
	for _, tc := range []struct{ proxy, address string }{
		{"http://dummy:dummy-password@proxy.example", "proxy.example:80"},
		{"https://proxy.example/", "proxy.example:443"},
		{"socks5://dummy:dummy-password@proxy.example", "proxy.example:1080"},
		{"socks5h://proxy.example", "proxy.example:1080"},
		{"https://proxy.example:8443", "proxy.example:8443"},
		{"socks5://[2001:db8::1]", "[2001:db8::1]:1080"},
		{"http://[fe80::1%25eth0]:8080", "[fe80::1%eth0]:8080"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			parsed, err := url.Parse(tc.proxy)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				calls++
				if network != "tcp" || address != tc.address {
					t.Fatalf("wrong TCP gateway: %s %s", network, address)
				}
				return nil, errors.New("dummy-password internal error")
			}
			result := doProxyCheck(context.Background(), tc.proxy, proxyGatewayAddress(parsed), dial)
			if calls != 1 || result.Status != "inconclusive" || result.Deletable || strings.Contains(result.Reason, "dummy-password") {
				t.Fatalf("internal errors must remain redacted and inconclusive: %+v, calls=%d", result, calls)
			}
		})
	}
}

func TestProxyCheckConnectionFailuresAreEligibleForManualRemoval(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		err          error
	}{
		{"deadline", "The proxy gateway TCP connection timed out", context.DeadlineExceeded},
		{"dns", "The proxy gateway hostname could not be resolved", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "dummy-password lookup failure", Name: "proxy.example", IsNotFound: true}}},
		{"dns-timeout", "The proxy gateway TCP connection timed out", &net.DNSError{Err: "dummy-password lookup timeout", Name: "proxy.example", IsTimeout: true}},
		{"refused", "The proxy gateway TCP connection failed", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("dummy-password connection refused")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := doProxyCheck(context.Background(), "socks5://dummy:dummy-password@proxy.example:1080", "proxy.example:1080",
				func(context.Context, string, string) (net.Conn, error) { return nil, tc.err })
			if result.Status != "failed" || !result.Deletable || result.Reason != tc.reason || result.GatewayIP != "" || strings.Contains(result.Reason, "dummy-password") {
				t.Fatalf("incorrect TCP failure classification: %+v", result)
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
	if err != nil || result.Status != "failed" || !result.Deletable || result.Reason != "The proxy gateway TCP connection failed" {
		t.Fatalf("refused gateway connection not eligible for removal: %+v, %v", result, err)
	}
}

func TestProxyCheckHonorsDeadlineAndReleasesPartialConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	local, remote := net.Pipe()
	defer remote.Close()
	started := time.Now()
	result := doProxyCheck(ctx, "https://proxy.example", "proxy.example:443", func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return local, ctx.Err()
	})
	if time.Since(started) > time.Second || result.Status != "failed" || !result.Deletable || result.Reason != "The proxy gateway TCP connection timed out" {
		t.Fatalf("incorrect deadline result: %+v", result)
	}
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if n, err := remote.Read(data[:]); n != 0 || err != io.EOF {
		t.Fatalf("a partially established connection was not closed: n=%d err=%v", n, err)
	}
	for _, dialErr := range []error{context.Canceled, errors.New("dummy-password internal failure"), nil} {
		result := doProxyCheck(context.Background(), "https://proxy.example", "proxy.example:443", func(context.Context, string, string) (net.Conn, error) { return nil, dialErr })
		if result.Status != "inconclusive" || result.Deletable {
			t.Fatalf("non-network or incomplete check marked gateway as failed: %+v", result)
		}
	}
}

func TestProxyCheckRejectsMalformedURLsWithoutEchoingSecrets(t *testing.T) {
	for _, proxy := range []string{"", "direct", "ftp://dummy:dummy-password@host:21", "http://dummy:dummy-password@host:80/path", "http://dummy:dummy-password@host:80?key=x", "http://dummy:dummy-password@host:80\r\nheader: x", "http://host:0", "http://host:65536", "http://host:bad", "http://" + strings.Repeat("x", 4096)} {
		result, err := CheckProxy(proxy)
		encoded, _ := json.Marshal(result)
		if err != nil || result.Status != "failed" || !result.Deletable || strings.Contains(string(encoded), "dummy-password") {
			t.Fatalf("invalid proxy accepted or credentials reflected: %q, %+v, %v", proxy, result, err)
		}
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
