package turnstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const probeEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// newProbeClient owns a transport for exactly one synchronous probe. Closing
// the body and transport releases the connection before the host call ends.
// In particular, rotating proxies get a new connection on every attempt.
func newProbeClient(proxy string) (*http.Client, *http.Transport, error) {
	transport := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: -1}).DialContext,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  25 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		// HTTP/1.1 avoids an HTTP/2 connection pool and its idle read loop.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err != nil || parsed.Hostname() == "" {
			return nil, nil, errors.New("invalid probe proxy")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
			parsed.Scheme = strings.ToLower(parsed.Scheme)
		default:
			return nil, nil, errors.New("unsupported probe proxy scheme")
		}
		// Go's transport performs SOCKS DNS resolution at the proxy for both
		// socks5 and socks5h, preserving the previous probe behavior. Explicit
		// ProxyURL also avoids HTTP(S)_PROXY/NO_PROXY changing the chosen exit.
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   25 * time.Second,
		// Do not forward an account token to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport, nil
}

func newProbeRequest(endpoint string, credential Credential, model string) (*http.Request, error) {
	payload, err := json.Marshal(map[string]any{
		"model": model, "stream": true, "store": false, "instructions": "",
		"input":     []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply OK."}}}},
		"reasoning": map[string]string{"effort": "low"}, "parallel_tool_calls": false,
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	var session [16]byte
	if _, err = rand.Read(session[:]); err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Originator", "codex-tui")
	request.Header.Set("Session-Id", fmt.Sprintf("%x", session))
	request.Header.Set("User-Agent", "codex-tui/0.154.0 (Linux; x86_64)")
	if credential.AccountID != "" {
		request.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	return request, nil
}

func runHTTPProbe(credential Credential, model, proxy string) (ProbeResponse, error) {
	client, transport, err := newProbeClient(proxy)
	if err != nil {
		return ProbeResponse{}, err
	}
	defer transport.CloseIdleConnections()
	var connectStatus atomic.Int32
	transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		connectStatus.Store(int32(response.StatusCode))
		return nil
	}
	response, err := doHTTPProbe(client, probeEndpoint, credential, model)
	if proxy != "" {
		response.ProxyFailure = response.Status == http.StatusProxyAuthRequired || probeProxyFailure(err, int(connectStatus.Load()))
	}
	return response, err
}

// Account HTTP failures never arrive here as transport errors. CONNECT errors
// have their own status: a gateway's policy/rate limit is not proof of a broken
// proxy. Only 407 is an explicit proxy-authentication failure. Local malformed
// requests, credential errors, and TLS trust errors are also not removal hints.
func probeProxyFailure(err error, connectStatus int) bool {
	if err == nil {
		return false
	}
	if connectStatus != 0 && connectStatus != http.StatusOK {
		return connectStatus == http.StatusProxyAuthRequired
	}
	if strings.Contains(err.Error(), "username/password authentication failed") || strings.Contains(err.Error(), "no acceptable authentication methods") {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) {
		return operationError.Op == "dial" || operationError.Op == "proxyconnect" || operationError.Op == "read" || operationError.Op == "write"
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func doHTTPProbe(client *http.Client, endpoint string, credential Credential, model string) (ProbeResponse, error) {
	request, err := newProbeRequest(endpoint, credential, model)
	if err != nil {
		return ProbeResponse{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return ProbeResponse{}, err
	}
	defer response.Body.Close()
	// The state is in the final upstream response headers. A CONNECT response
	// from an HTTP(S) proxy is handled by Transport and cannot be harvested.
	// Legacy mode closes immediately. Strict mode reads only this synthetic
	// probe's bounded response until its completion event, under the same total
	// Client.Timeout. Business responses are never buffered or inspected here.
	result := ProbeResponse{Status: response.StatusCode, Value: strings.TrimSpace(response.Header.Get(Header)), Cookies: responseCookies(response.Header),
		ContentType: response.Header.Get("Content-Type")}
	if credential.ProbeVerifyCompletion && response.StatusCode == http.StatusOK {
		result.CompletionFailure = verifyProbeCompletion(response)
	}
	if response.StatusCode == http.StatusForbidden {
		result.ExitBlocked = exitBlocked(response)
	}
	return result, nil
}

// exitBlocked reads at most 4 KiB of a 403 to tell an exit-level block (a
// Cloudflare challenge page or an unsupported country) from an account refusal.
func exitBlocked(response *http.Response) bool {
	if response.Header.Get("Cf-Mitigated") != "" {
		return true
	}
	if kind := strings.ToLower(response.Header.Get("Content-Type")); strings.HasPrefix(kind, "text/html") {
		return true
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	text := strings.ToLower(string(body))
	return strings.Contains(text, "unsupported_country") || strings.Contains(text, "country, region, or territory not supported")
}
