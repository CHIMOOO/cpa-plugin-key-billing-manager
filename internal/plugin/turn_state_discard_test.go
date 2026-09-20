package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func prepareDiscardTemplate(t *testing.T, a *App) (string, string) {
	t.Helper()
	if err := a.turnState.Update([]byte(`{"enabled":true,"models":["model-a"],"probe_accounts":["dummy-account"]}`)); err != nil {
		t.Fatal(err)
	}
	value := rpcTurnStateTemplate()
	a.turnState.Before("learn-before-discard", "dummy-account", "model-a", nil)
	if err := a.turnState.Learn("learn-before-discard", "", "", http.Header{turnstate.Header: {value}}); err != nil {
		t.Fatal(err)
	}
	return a.turnState.Status().Templates[0].Fingerprint, value
}

func TestDiscardManagementConfirmsExactTemplateAndReturnsCompleteStatus(t *testing.T) {
	a := newConfiguredApp(t)
	fingerprint, value := prepareDiscardTemplate(t, a)
	a.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("discard unexpectedly called host %s", method)
		}
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{{ID: "dummy-account", AuthIndex: "dummy-index", Provider: "codex"}}}), nil
	})
	body := mustMarshal(t, map[string]any{"account": "dummy-account", "model": "model-a", "fingerprint": fingerprint, "confirm": true})
	response := a.routeManagement(ManagementRequest{Method: http.MethodPost, Body: body}, routeTurnStateDiscard)
	if response.StatusCode != http.StatusOK || response.Headers.Get("Cache-Control") != "private, no-store" || strings.Contains(string(response.Body), value) {
		t.Fatalf("discard response: %d %s", response.StatusCode, response.Body)
	}
	var status turnStateStatus
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Templates) != 0 || len(status.ProbeAccounts) != 1 || status.Runner.InFlight || status.Runner.Enabled {
		t.Fatal("discard did not return completed full UI status", status)
	}
	a.turnState.Before("learn-after-discard", "dummy-account", "model-a", nil)
	if err := a.turnState.Learn("learn-after-discard", "", "", http.Header{turnstate.Header: {value}}); err != nil {
		t.Fatal(err)
	}
	if len(a.turnState.Status().Templates) != 0 {
		t.Fatal("discarded template immediately reappeared")
	}
	stale := a.routeManagement(ManagementRequest{Method: http.MethodPost, Body: body}, routeTurnStateDiscard)
	if stale.StatusCode != http.StatusConflict || !strings.Contains(string(stale.Body), "template_changed") {
		t.Fatal("stale button did not conflict", stale.StatusCode, string(stale.Body))
	}
}

func TestDiscardManagementRejectsUnconfirmedInvalidAndActiveCollector(t *testing.T) {
	a := newConfiguredApp(t)
	fingerprint, _ := prepareDiscardTemplate(t, a)
	a.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Fatal("rejected discard made a host call")
		return nil, nil
	})
	for _, body := range []string{
		`{"account":"dummy-account","model":"model-a"}`,
		`{"account":"dummy-account","model":"model-a","confirm":true,"fingerprint":"invalid"}`,
		`{"account":"dummy-account","model":"model-a","confirm":true,"unknown":true}`,
	} {
		response := a.routeManagement(ManagementRequest{Method: http.MethodPost, Body: []byte(body)}, routeTurnStateDiscard)
		if response.StatusCode != http.StatusBadRequest || response.Headers.Get("Cache-Control") != "private, no-store" || len(a.turnState.Status().Templates) != 1 {
			t.Fatal("invalid discard changed state", response.StatusCode, string(response.Body))
		}
	}
	a.turnStateRunner.mu.Lock()
	a.turnStateRunner.control.Enabled = true
	a.turnStateRunner.mu.Unlock()
	response := a.routeManagement(ManagementRequest{Method: http.MethodPost, Body: mustMarshal(t, map[string]any{"account": "dummy-account", "model": "model-a", "fingerprint": fingerprint, "confirm": true})}, routeTurnStateDiscard)
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(response.Body), "runner_active") || len(a.turnState.Status().Templates) != 1 {
		t.Fatal("active collector permitted discard", response.StatusCode, string(response.Body))
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := a.routeResource(ManagementRequest{Method: method, Headers: http.Header{"Authorization": {"Bearer dummy-downstream-key"}}}, routeTurnStateDiscard)
		if response.StatusCode != http.StatusNotFound {
			t.Fatal("private discard route exposed to downstream key", response.StatusCode)
		}
	}
}
