package plugin

import (
	"errors"
	"net/http"

	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

// Saved proxy credentials are intentionally returned only through these
// authenticated management routes, never through public resource endpoints.
func (a *App) readTurnStateProxies(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Pool     string `json:"pool"`
		Offset   int    `json:"offset"`
		Revision string `json:"revision"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_proxy_page", messages.FromError(err))
	}
	page, err := a.turnState.ReadProxyPage(input.Pool, input.Offset, input.Revision)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, turnstate.ErrProxyConfigChanged) {
			status = http.StatusConflict
		}
		return jsonMessageError(status, "invalid_proxy_page", messages.FromError(err))
	}
	return JSONResponse(http.StatusOK, page)
}

func (a *App) testTurnStateProxy(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Proxy string `json:"proxy"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_proxy_test", messages.FromError(err))
	}
	result, err := turnstate.CheckProxy(input.Proxy)
	if err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_proxy_test", messages.FromError(err))
	}
	return JSONResponse(http.StatusOK, result)
}
