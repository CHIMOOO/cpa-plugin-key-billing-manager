package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestProxyManagementReadIsExplicitBoundedAndPrivate(t *testing.T) {
	app := newConfiguredApp(t)
	app.hostCaller = func(string, any) (json.RawMessage, error) {
		t.Fatal("proxy editor unexpectedly read account credentials or host state")
		return nil, nil
	}
	if err := app.turnState.Update([]byte(`{"probe_proxies":["http://dummy:dummy-password@proxy.example:80"]}`)); err != nil {
		t.Fatal(err)
	}
	response := app.routeManagement(ManagementRequest{Method: http.MethodPost, Body: []byte(`{"pool":"static","offset":0}`)}, routeTurnStateProxiesRead)
	if response.StatusCode != http.StatusOK || response.Headers.Get("Cache-Control") != "private, no-store" || !strings.Contains(string(response.Body), "dummy-password") || len(response.Body) > turnstate.MaxProxyPageBytes {
		t.Fatalf("explicit admin read failed: status=%d headers=%v", response.StatusCode, response.Headers)
	}
	var page turnstate.ProxyPage
	if err := json.Unmarshal(response.Body, &page); err != nil {
		t.Fatal(err)
	}
	wrapped := app.routeManagement(ManagementRequest{Method: http.MethodPost, Query: url.Values{"view": {"1"}},
		Body: []byte(`{"data":{"pool":"static","offset":0}}`)}, routeTurnStateProxiesRead)
	if wrapped.StatusCode != http.StatusOK || wrapped.Headers.Get("Cache-Control") != "private, no-store" || !strings.Contains(string(wrapped.Body), "dummy-password") {
		t.Fatal("management view wrapper discarded the private response cache policy")
	}
	if err := app.turnState.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	response = app.routeManagement(ManagementRequest{Method: http.MethodPost, Body: mustMarshal(t, map[string]any{"pool": "rotating", "revision": page.Revision})}, routeTurnStateProxiesRead)
	if response.StatusCode != http.StatusConflict || strings.Contains(string(response.Body), "dummy-password") {
		t.Fatalf("stale proxy read = %d", response.StatusCode)
	}
	for _, route := range []string{routeTurnStateProxiesRead, routeTurnStateProxiesTest, routeTurnStateProbeProgress} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			response = app.routeResource(ManagementRequest{Method: method, Headers: http.Header{"Authorization": {"Bearer dummy-downstream-key"}}}, route)
			if response.StatusCode != http.StatusNotFound || strings.Contains(string(response.Body), "dummy-password") {
				t.Fatalf("proxy route exposed through downstream resource: %s %s, status=%d", method, route, response.StatusCode)
			}
		}
	}
	for _, body := range []string{`{"pool":"static","unknown":"dummy-password"}`, `{"pool":"static"} {}`, strings.Repeat("x", 20<<10+1)} {
		response = app.readTurnStateProxies(ManagementRequest{Body: []byte(body)})
		if response.StatusCode != http.StatusBadRequest || response.Headers.Get("Cache-Control") != "private, no-store" || strings.Contains(string(response.Body), "dummy-password") {
			t.Fatal("malformed proxy read was accepted or echoed input")
		}
	}
}

func TestProxyManagementCheckIsIndependentAndRedacted(t *testing.T) {
	app := newConfiguredApp(t)
	app.hostCaller = func(string, any) (json.RawMessage, error) {
		t.Fatal("proxy connectivity test unexpectedly read an account or invoked CPA")
		return nil, nil
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("TCP gateway check must not send an HTTP or CONNECT request")
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	before, _ := json.Marshal(app.turnState.Status())
	response := app.routeManagement(ManagementRequest{Method: http.MethodPost, Body: mustMarshal(t, map[string]string{
		"proxy": strings.Replace(proxy.URL, "://", "://dummy:dummy-password@", 1),
	})}, routeTurnStateProxiesTest)
	if response.StatusCode != http.StatusOK || response.Headers.Get("Cache-Control") != "private, no-store" || strings.Contains(string(response.Body), "dummy-password") {
		t.Fatalf("proxy test response = %d, headers=%v", response.StatusCode, response.Headers)
	}
	var result turnstate.ProxyCheckResult
	if err := json.Unmarshal(response.Body, &result); err != nil || result.Status != "reachable" || result.Deletable || result.HTTPStatus != 0 || result.GatewayIP != "127.0.0.1" || result.IP != "" {
		t.Fatalf("proxy test result=%+v, err=%v", result, err)
	}
	// Status includes the current server time, so compare the state-bearing
	// members instead of demanding byte-identical response timestamps.
	var beforeStatus, afterStatus turnstate.Status
	_ = json.Unmarshal(before, &beforeStatus)
	afterStatus = app.turnState.Status()
	if afterStatus.Counters != beforeStatus.Counters || afterStatus.ProxyCounts["static"] != beforeStatus.ProxyCounts["static"] || afterStatus.ProxyCounts["rotating"] != beforeStatus.ProxyCounts["rotating"] || len(afterStatus.Templates) != len(beforeStatus.Templates) {
		t.Fatal("proxy connectivity test changed collection state")
	}
	for _, body := range []string{`{"proxy":"http://host:80","account":"dummy-password"}`, `{"proxy":"http://host:80"} {}`} {
		response = app.testTurnStateProxy(ManagementRequest{Body: []byte(body)})
		if response.StatusCode != http.StatusBadRequest || response.Headers.Get("Cache-Control") != "private, no-store" || strings.Contains(string(response.Body), "dummy-password") {
			t.Fatal("invalid connectivity request was accepted or leaked its input")
		}
	}
}
