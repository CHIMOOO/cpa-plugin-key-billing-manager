package plugin

import (
	"net/http"
	"strconv"
	"strings"

	"cpa-key-billing/internal/billing"
)

func (a *App) listPrices(req ManagementRequest, access viewAccess) ManagementResponse {
	models := req.Query["model"]
	includeCustom := !access.APIKey
	if raw := req.Query.Get("include_custom"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return viewJSONError(access, http.StatusBadRequest, "invalid", "include_custom must be true or false")
		}
		includeCustom = includeCustom && value
	}
	prices, err := a.store.ModelPriceRows(models, includeCustom)
	if err != nil {
		return viewErrorResponse(access, err)
	}
	return viewJSON(access, http.StatusOK, prices)
}

func (a *App) putPrices(req ManagementRequest) ManagementResponse {
	var price billing.CustomPrice
	if errDecode := decodeStrict(req.Body, &price); errDecode != nil {
		return errorResponse(errDecode)
	}
	stored, errUpsert := a.store.UpsertPrice(price)
	if errUpsert != nil {
		return errorResponse(errUpsert)
	}
	return JSONResponse(http.StatusOK, map[string]any{"price": stored})
}

func (a *App) searchReferencePrices(req ManagementRequest) ManagementResponse {
	limit := 20
	if raw := strings.TrimSpace(req.Query.Get("limit")); raw != "" {
		parsed, errParse := strconv.Atoi(raw)
		if errParse != nil || parsed < 1 || parsed > 50 {
			return JSONError(http.StatusBadRequest, "invalid", "Limit must be an integer from 1 to 50")
		}
		limit = parsed
	}
	prices, err := a.store.SearchReferencePrices(req.Query.Get("q"), limit)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"prices": prices})
}

func (a *App) refreshReferencePrices() ManagementResponse {
	result, errRefresh := a.store.RefreshReferencePrices()
	if errRefresh != nil {
		return errorResponse(errRefresh)
	}
	return JSONResponse(http.StatusOK, result)
}

func (a *App) deletePrice(req ManagementRequest) ManagementResponse {
	if err := a.store.DeletePrice(req.Query.Get("model_id")); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(200, map[string]any{"deleted": req.Query.Get("model_id")})
}

func (a *App) referencePriceStatus() ManagementResponse {
	return JSONResponse(200, map[string]any{"metadata": a.store.ReferencePriceMetadata()})
}
