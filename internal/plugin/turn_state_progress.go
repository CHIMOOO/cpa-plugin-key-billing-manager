package plugin

import "net/http"

// This read-only endpoint intentionally avoids the account inventory and full
// configuration so the UI can display the active exit during a bounded probe.
func (a *App) getTurnStateProbeProgress(_ ManagementRequest) ManagementResponse {
	response := JSONResponse(http.StatusOK, a.turnState.ProbeProgress())
	response.Headers.Set("Cache-Control", "private, no-store")
	return response
}
