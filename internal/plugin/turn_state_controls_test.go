package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestTurnStateSelfTestPinsHostAccountAndNeverHarvests(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"models":["model-a"],"probe_accounts":["dummy-account"]}`)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	app.SetHostCaller(func(method string, input any) (json.RawMessage, error) {
		calls++
		if !app.turnStateRunner.status().InFlight {
			t.Fatal("self-test did not reserve the manual diagnostic gate")
		}
		if finish, allowed := app.beginManualTurnStateProbe(); allowed {
			finish()
			t.Fatal("a second diagnostic entered while self-test was executing")
		}
		if method == hostAuthList {
			return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{{ID: "dummy-account", AuthIndex: "dummy-index", Provider: "codex"}}}), nil
		}
		if method != "host.model.execute" {
			t.Fatalf("self-test modified host: %s", method)
		}
		req := input.(turnStateSelfTestRequest)
		if req.AuthID != "dummy-account" || req.ForcedProvider != "codex" || req.Model != "model-a" || req.HostCallbackID != "dummy-callback" || req.EntryProtocol != "openai-responses" || req.ExitProtocol != "openai-responses" || req.Headers.Get(turnstate.Header) != "" {
			t.Fatalf("incorrect host request=%+v", req)
		}
		return mustMarshal(t, map[string]any{"status_code": 200, "headers": http.Header{turnstate.Header: {rpcTurnStateTemplate()}, "Authorization": {"Bearer dummy-secret"}}, "body": []byte("dummy-secret")}), nil
	})
	response := app.selfTestTurnState(ManagementRequest{HostCallbackID: "dummy-callback", Body: []byte(`{"account":"dummy-account","model":"model-a","confirm":true}`)})
	var result turnStateSelfTestResponse
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &result) != nil || !result.Reached || result.Harvested || result.Status != 200 || result.Length != 292 || calls != 2 {
		t.Fatalf("self-test response=%d %s", response.StatusCode, response.Body)
	}
	if len(app.turnState.Status().Templates) != 0 || app.turnState.Status().ProbeStats.Attempts != 0 || strings.Contains(string(response.Body), "dummy-secret") || strings.Contains(string(response.Body), rpcTurnStateTemplate()) {
		t.Fatal("self-test harvested or leaked response")
	}
	if app.turnStateRunner.status().InFlight {
		t.Fatal("finished self-test retained the manual diagnostic gate")
	}
}

func TestTurnStateSelfTestRejectsRunningAndDrainingCollector(t *testing.T) {
	for _, test := range []struct {
		name     string
		enabled  bool
		inFlight bool
	}{
		{"enabled", true, false},
		{"running", true, true},
		{"draining", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newConfiguredApp(t)
			app.turnStateRunner.mu.Lock()
			app.turnStateRunner.control.Enabled = test.enabled
			app.turnStateRunner.inFlight = test.inFlight
			app.turnStateRunner.mu.Unlock()
			app.SetHostCaller(func(string, any) (json.RawMessage, error) {
				t.Fatal("blocked diagnostic made a host call")
				return nil, nil
			})
			unconfirmed := app.selfTestTurnState(ManagementRequest{Body: []byte(`{"account":"dummy-account","model":"model-a"}`)})
			if unconfirmed.StatusCode != http.StatusBadRequest {
				t.Fatal("confirmation must be validated before reserving the gate")
			}
			response := app.selfTestTurnState(ManagementRequest{Body: []byte(`{"account":"dummy-account","model":"model-a","confirm":true}`)})
			if response.StatusCode != http.StatusConflict || !strings.Contains(string(response.Body), "runner_active") {
				t.Fatalf("collector/self-test overlap accepted: %d %s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestTurnStateControlsRequireConfirmationAndKeepErrorsPrivate(t *testing.T) {
	app := newConfiguredApp(t)
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Fatal("unconfirmed action made a host call")
		return nil, nil
	})
	for _, route := range []string{routeTurnStateSelfTest, routeTurnStateCooldowns} {
		response := app.routeManagement(ManagementRequest{Method: http.MethodPost, Body: []byte(`{"account":"dummy","model":"gpt-6-astra"}`)}, route)
		if response.StatusCode != 400 || response.Headers.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("unconfirmed=%d %s", response.StatusCode, response.Body)
		}
	}
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method == hostAuthList {
			return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{{ID: "dummy", AuthIndex: "dummy", Provider: "codex"}}}), nil
		}
		return nil, errors.New("Bearer dummy-secret rejected")
	})
	response := app.selfTestTurnState(ManagementRequest{Body: []byte(`{"account":"dummy","model":"gpt-6-astra","confirm":true}`)})
	var result turnStateSelfTestResponse
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "dummy-secret") || json.Unmarshal(response.Body, &result) != nil || result.Reached || result.Status != 0 {
		t.Fatalf("host error leaked or invented upstream response=%s", response.Body)
	}
	for _, route := range []string{routeTurnStateSelfTest, routeTurnStateCooldowns, routePersistence} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			response = app.routeResource(ManagementRequest{Method: method, Headers: http.Header{"Authorization": {"Bearer dummy-downstream-key"}}}, route)
			if response.StatusCode != 404 {
				t.Fatalf("private control exposed at resource route: %s %s", method, route)
			}
		}
	}
}

func TestTurnStateSelfTestIncludesVerifiedHistoricalBucketsWithoutInventoryEntry(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"enabled":true,"models":["model-a"]}`)); err != nil {
		t.Fatal(err)
	}
	app.turnState.Before("known-codex", "dummy-config-credential", "historical-model", nil)
	if err := app.turnState.Learn("known-codex", "dummy-config-credential", "historical-model", http.Header{turnstate.Header: {rpcTurnStateTemplate()}}); err != nil {
		t.Fatal(err)
	}
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		if method == hostAuthList {
			return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{}}), nil
		}
		request := payload.(turnStateSelfTestRequest)
		if request.AuthID != "dummy-config-credential" || request.Model != "historical-model" || request.ForcedProvider != "codex" {
			t.Fatal("historical bucket lost its exact attribution")
		}
		return mustMarshal(t, map[string]any{"status_code": 200}), nil
	})
	req := ManagementRequest{Body: []byte(`{"account":"dummy-config-credential","model":"historical-model","confirm":true}`)}
	response := app.selfTestTurnState(req)
	var result turnStateSelfTestResponse
	if json.Unmarshal(response.Body, &result) != nil || !result.Reached || len(app.turnState.Status().Templates) != 1 {
		t.Fatalf("historical self-test=%s", response.Body)
	}
	app.SetHostCaller(nil)
	if response = app.selfTestTurnState(req); response.StatusCode != http.StatusBadGateway {
		t.Fatal("known bucket bypassed missing host callback table")
	}
}
