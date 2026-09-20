package turnstate

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

const proxyCheckTimeout = 5 * time.Second

type ProxyCheckResult struct {
	Proxy         string           `json:"proxy"`
	Status        string           `json:"status"`
	IP            string           `json:"ip,omitempty"` // Legacy exit IP; a TCP gateway check does not populate it.
	GatewayIP     string           `json:"gateway_ip,omitempty"`
	LatencyMS     int64            `json:"latency_ms"`
	HTTPStatus    int              `json:"http_status,omitempty"` // Retained for older clients; TCP checks do not use HTTP.
	Reason        string           `json:"reason"`
	ReasonMessage messages.Message `json:"reason_message,omitzero"`
	Deletable     bool             `json:"deletable"`
}

// CheckProxy only establishes and closes a TCP connection to the proxy gateway.
// It sends no HTTP, TLS, SOCKS, or authentication payload, and does not contact
// an upstream service or an IP lookup service. A reachable gateway does not
// verify the proxy credentials or its ability to forward collection requests.
// It never changes proxy pools, accounts, or collection cooldowns.
func CheckProxy(proxy string) (ProxyCheckResult, error) {
	proxy = strings.TrimSpace(proxy)
	if len(proxy) > 4096 || proxy == "" {
		return invalidProxyCheck(messages.Errorf("Enter a complete proxy URL to test")), nil
	}
	if err := validateProxy(proxy); err != nil {
		return invalidProxyCheck(err), nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil {
		return invalidProxyCheck(messages.Errorf("Enter a complete proxy URL to test")), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyCheckTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: proxyCheckTimeout, KeepAlive: -1}
	return doProxyCheck(ctx, proxy, proxyGatewayAddress(parsed), dialer.DialContext), nil
}

func proxyGatewayAddress(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		// Match net/http's proxy defaults used by the collection transport.
		switch proxy.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}

func invalidProxyCheck(err error) ProxyCheckResult {
	detail := messages.FromError(err)
	return ProxyCheckResult{Proxy: "invalid proxy", Status: "failed", Deletable: true,
		Reason: detail.Text, ReasonMessage: detail}
}

func doProxyCheck(ctx context.Context, proxy, address string, dial func(context.Context, string, string) (net.Conn, error)) (result ProxyCheckResult) {
	started := time.Now()
	result = ProxyCheckResult{Proxy: maskProxy(proxy), Status: "inconclusive"}
	defer func() {
		result.LatencyMS = time.Since(started).Milliseconds()
		result.ReasonMessage = messages.Literal(result.Reason)
	}()
	connection, err := dial(ctx, "tcp", address)
	if connection != nil {
		defer connection.Close()
	}
	if err != nil {
		// Dial errors may include input credentials or arbitrary peer details.
		// Return fixed diagnostic categories only. A failed connection is eligible
		// for explicit manual removal, not proof that the gateway is always down.
		var networkError net.Error
		var dnsError *net.DNSError
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			result.Status, result.Deletable, result.Reason = "failed", true, "The proxy gateway TCP connection timed out"
		} else if errors.As(err, &dnsError) {
			result.Status, result.Deletable, result.Reason = "failed", true, "The proxy gateway hostname could not be resolved"
		} else if errors.As(err, &networkError) {
			result.Status, result.Deletable, result.Reason = "failed", true, "The proxy gateway TCP connection failed"
		} else {
			result.Reason = "The proxy gateway TCP connection test could not be completed"
		}
		return result
	}
	if connection == nil {
		result.Reason = "The proxy gateway TCP connection test could not be completed"
		return result
	}
	result.Status, result.Reason = "reachable", "The proxy gateway TCP connection succeeded; authentication and outbound access were not tested"
	if peer, ok := connection.RemoteAddr().(*net.TCPAddr); ok && peer.IP != nil {
		result.GatewayIP = peer.IP.String()
	}
	return result
}
