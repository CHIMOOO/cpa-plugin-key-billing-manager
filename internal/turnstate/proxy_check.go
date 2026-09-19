package turnstate

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"cpa-key-billing/internal/messages"
)

const proxyTraceEndpoint = "https://chatgpt.com/cdn-cgi/trace"
const proxyCheckBodyLimit = 4 << 10

type ProxyCheckResult struct {
	Proxy         string           `json:"proxy"`
	Status        string           `json:"status"`
	IP            string           `json:"ip,omitempty"`
	LatencyMS     int64            `json:"latency_ms"`
	HTTPStatus    int              `json:"http_status,omitempty"`
	Reason        string           `json:"reason"`
	ReasonMessage messages.Message `json:"reason_message,omitzero"`
	Deletable     bool             `json:"deletable"`
}

// CheckProxy makes one independent HTTPS connectivity request, with neither
// OAuth nor downstream credentials. It never changes pools or bucket cooldowns.
// A reachable result describes this connection only; rotating proxies can use
// another exit on the following collection request.
func CheckProxy(proxy string) (ProxyCheckResult, error) {
	proxy = strings.TrimSpace(proxy)
	if len(proxy) > 4096 || proxy == "" {
		return invalidProxyCheck(messages.Errorf("Enter a complete proxy URL to test")), nil
	}
	if err := validateProxy(proxy); err != nil {
		return invalidProxyCheck(err), nil
	}
	client, transport, err := newProbeClient(proxy)
	if err != nil {
		return ProxyCheckResult{}, messages.Errorf("Enter a complete proxy URL to test")
	}
	defer transport.CloseIdleConnections()
	client.Timeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.MaxResponseHeaderBytes = 16 << 10
	var connectStatus atomic.Int32
	transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		connectStatus.Store(int32(response.StatusCode))
		return nil
	}
	return doProxyCheck(client, probeEndpoint, proxyTraceEndpoint, proxy, &connectStatus), nil
}

func invalidProxyCheck(err error) ProxyCheckResult {
	detail := messages.FromError(err)
	return ProxyCheckResult{Proxy: "invalid proxy", Status: "failed", Deletable: true,
		Reason: detail.Text, ReasonMessage: detail}
}

func doProxyCheck(client *http.Client, endpoint, traceEndpoint, proxy string, connectStatus *atomic.Int32) (result ProxyCheckResult) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result = ProxyCheckResult{Proxy: maskProxy(proxy), Status: "inconclusive"}
	defer func() {
		result.LatencyMS = time.Since(started).Milliseconds()
		result.ReasonMessage = messages.Literal(result.Reason)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		result.Reason = "The proxy connectivity test could not be started"
		return result
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "cpa-key-billing-proxy-check")
	response, err := client.Do(request)
	if err != nil {
		// Client errors can contain the entire URL and its credentials. Return
		// only fixed classifications, never the original error or response body.
		// A canceled Do can finish before the Transport's CONNECT callback.
		// Snapshot atomically because that callback runs in its dial goroutine.
		status := 0
		if connectStatus != nil {
			status = int(connectStatus.Load())
		}
		if status != 0 && status != http.StatusOK {
			result.HTTPStatus = status
			if status == http.StatusProxyAuthRequired {
				result.Status, result.Deletable, result.Reason = "failed", true, "Proxy authentication failed"
			} else {
				result.Reason = "The proxy rejected the connectivity test; the result is inconclusive"
			}
			return result
		}
		// An endpoint timeout or TLS policy failure does not establish that the
		// proxy itself is dead. Only explicit proxy connection/authentication
		// failures are eligible for the UI's optional removal action.
		var operationError *net.OpError
		if errors.As(err, &operationError) && (operationError.Op == "dial" || operationError.Op == "proxyconnect") {
			result.Status, result.Deletable = "failed", true
		}
		if strings.Contains(err.Error(), "username/password authentication failed") || strings.Contains(err.Error(), "no acceptable authentication methods") {
			result.Status, result.Deletable, result.Reason = "failed", true, "Proxy authentication failed"
			return result
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			result.Reason = "The proxy connectivity test timed out"
		} else {
			result.Reason = "The proxy could not establish a trusted HTTPS connection"
		}
		return result
	}
	// The unauthenticated API request is only a reachability check. Closing at
	// headers prevents an unexpected streaming body from prolonging the test.
	_ = response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode == http.StatusProxyAuthRequired {
		result.Status, result.Deletable, result.Reason = "failed", true, "Proxy authentication failed"
		return result
	}
	if response.StatusCode != http.StatusUnauthorized && (response.StatusCode < 200 || response.StatusCode >= 300) {
		result.Reason = "The Codex API rejected the connectivity test; the result is inconclusive"
		return result
	}
	result.Status, result.Reason = "reachable", "The proxy reached the Codex API without using an account"
	// Trace is best effort and uses another fresh connection. Its IP is only
	// evidence for this trace request, never a claim about a subsequent harvest.
	traceCtx, traceCancel := context.WithTimeout(ctx, 2*time.Second)
	defer traceCancel()
	result.IP = readProxyTraceIP(traceCtx, client, traceEndpoint)
	return result
}

func readProxyTraceIP(ctx context.Context, client *http.Client, endpoint string) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	request.Header.Set("User-Agent", "cpa-key-billing-proxy-check")
	response, err := client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, proxyCheckBodyLimit+1))
	if err != nil || len(raw) > proxyCheckBodyLimit {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		if ip := net.ParseIP(strings.TrimSpace(strings.TrimPrefix(line, "ip="))); ip != nil {
			return ip.String()
		}
	}
	return ""
}
