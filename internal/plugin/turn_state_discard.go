package plugin

import (
	"errors"
	"net/http"

	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

func (a *App) discardTurnState(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Account     string `json:"account"`
		Model       string `json:"model"`
		Fingerprint string `json:"fingerprint"`
		Confirm     bool   `json:"confirm"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	if !input.Confirm {
		return JSONError(http.StatusBadRequest, "confirmation_required", "Confirm discarding this exact template before continuing")
	}
	finish, allowed := a.beginManualTurnStateProbe()
	if !allowed {
		return JSONError(http.StatusConflict, "runner_active", "Stop server collection before discarding a template")
	}
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	if err := a.turnState.Discard(input.Account, input.Model, input.Fingerprint); err != nil {
		status, code := http.StatusInternalServerError, "turn_state_write_failed"
		switch {
		case errors.Is(err, turnstate.ErrInvalidDiscard):
			status, code = http.StatusBadRequest, "invalid_turn_state"
		case errors.Is(err, turnstate.ErrTemplateChanged):
			status, code = http.StatusConflict, "template_changed"
		case errors.Is(err, turnstate.ErrDiscardCapacity):
			status, code = http.StatusConflict, "discard_capacity"
		}
		return jsonMessageError(status, code, messages.FromError(err))
	}
	a.turnStateRunner.requestCheck()
	finish()
	finish = nil
	return a.getTurnState(req)
}
