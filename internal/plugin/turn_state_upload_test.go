package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestTurnStateUploadManagementRoutes(t *testing.T) {
	app := newConfiguredApp(t)
	call := func(method, path string, payload any, want int) ManagementResponse {
		t.Helper()
		response := app.routeManagement(ManagementRequest{Method: method, Body: mustMarshal(t, payload)}, path)
		if response.StatusCode != want {
			t.Fatalf("%s %s status=%d, want %d", method, path, response.StatusCode, want)
		}
		if strings.Contains(string(response.Body), "dummy-password") {
			t.Fatal("management response leaked a proxy password")
		}
		return response
	}
	raw := []byte(`{"enabled":true,"probe_proxies":["http://dummy-user:dummy-password@proxy.example:80"]}`)
	response := call(http.MethodPost, routeTurnStateUpload, map[string]int{"size": len(raw)}, http.StatusOK)
	var upload turnstate.ConfigUploadStatus
	if err := json.Unmarshal(response.Body, &upload); err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, routeTurnStateUploadCommit, map[string]string{"id": upload.ID}, http.StatusBadRequest)
	call(http.MethodPatch, routeTurnStateUpload, map[string]any{"id": upload.ID, "offset": 0, "data": raw}, http.StatusOK)
	if app.turnState.Enabled() {
		t.Fatal("staging enabled Turn State before commit")
	}
	call(http.MethodPost, routeTurnStateUploadCommit, map[string]string{"id": upload.ID}, http.StatusOK)
	if !app.turnState.Enabled() || app.turnState.Status().ProxyCounts["static"] != 1 {
		t.Fatal("commit did not apply staged settings")
	}
	call(http.MethodDelete, routeTurnStateUpload, map[string]string{"id": upload.ID}, http.StatusOK)
	call(http.MethodPost, routeTurnStateUploadCommit, map[string]string{"id": upload.ID}, http.StatusBadRequest)
	call(http.MethodPost, routeTurnStateUpload, map[string]any{"size": 2, "unknown": true}, http.StatusBadRequest)
	call(http.MethodPatch, routeTurnStateUpload, map[string]any{"id": upload.ID, "offset": 0, "data": "not-base64!"}, http.StatusBadRequest)
	response = app.beginTurnStateUpload(ManagementRequest{Body: make([]byte, 20<<10+1)})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "message_key") {
		t.Fatal("oversized request lacks a safe translated validation error")
	}
}
