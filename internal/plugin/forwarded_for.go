package plugin

import (
	"net/http"
	"regexp"
	"strings"

	"cpa-key-billing/internal/billing"
)

const (
	forwardedForHeader           = "X-Forwarded-For"
	maxForwardedForLogModelBytes = 256
)

// Unlike secretLikeToken, this leaves long dated model names readable.
var keyPrefixedToken = regexp.MustCompile(`(?i)(?:sk|key|token)-[a-z0-9_\-]{4,}`)

// forwardedForLogModel makes a client-chosen model name safe for a single,
// bounded log line: control characters are dropped and key-like tokens masked.
func forwardedForLogModel(model string) string {
	value := cleanText(keyPrefixedToken.ReplaceAllStringFunc(model, billing.PreviewKey))
	if len(value) > maxForwardedForLogModelBytes {
		value = strings.ToValidUTF8(value[:maxForwardedForLogModelBytes], "") + "…"
	}
	return value
}

func (a *App) setForwardedForBlock(req ManagementRequest) ManagementResponse {
	var body struct {
		Enabled       *bool     `json:"enabled"`
		ModelKeywords *[]string `json:"model_keywords"`
		Message       *string   `json:"message"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	if body.Enabled == nil {
		return JSONError(http.StatusBadRequest, "invalid", "enabled 为必填布尔值")
	}
	if body.ModelKeywords == nil || *body.ModelKeywords == nil {
		return JSONError(http.StatusBadRequest, "invalid", "model_keywords 必须是字符串数组；显式传入 [] 才会清空关键词")
	}
	settings := billing.ForwardedForBlock{Enabled: *body.Enabled, ModelKeywords: *body.ModelKeywords}
	if body.Message != nil {
		settings.Message = *body.Message
	}
	saved, err := a.store.SetForwardedForBlock(settings)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"forwarded_for_block": saved})
}

// carriesForwardedFor reports whether the client sent X-Forwarded-For with any
// value, including an empty one. Only its presence matters; the addresses are
// never read.
func carriesForwardedFor(headers http.Header) bool {
	if len(headers[forwardedForHeader]) > 0 {
		return true
	}
	for name, values := range headers {
		if len(values) > 0 && strings.EqualFold(name, forwardedForHeader) {
			return true
		}
	}
	return false
}
