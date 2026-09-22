package plugin

import (
	"net/http"
	"strconv"
	"strings"

	"cpa-key-billing/internal/turnstate"
)

// turnStateJournalPreview is how many recent entries the State tab shows.
const turnStateJournalPreview = 100

// The diagnostic journal lives in the State manager's memory. These routes
// only read it, switch recording and clear it; they never probe or persist.
func (a *App) getTurnStateJournal(req ManagementRequest) ManagementResponse {
	tail := turnStateJournalPreview
	if raw := strings.TrimSpace(req.Query.Get("tail")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return JSONError(http.StatusBadRequest, "invalid_turn_state_journal", "Invalid diagnostic log request")
		}
		tail = value
	}
	return turnStateJournalResponse(a.turnState.Journal(tail))
}

func (a *App) setTurnStateJournal(req ManagementRequest) ManagementResponse {
	var input struct {
		Recording *bool `json:"recording"`
	}
	if len(req.Body) > 1024 || decodeStrict(req.Body, &input) != nil || input.Recording == nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state_journal", "Invalid diagnostic log request")
	}
	a.turnState.SetJournalRecording(*input.Recording)
	return turnStateJournalResponse(a.turnState.Journal(turnStateJournalPreview))
}

func (a *App) clearTurnStateJournal(ManagementRequest) ManagementResponse {
	a.turnState.ClearJournal()
	return turnStateJournalResponse(a.turnState.Journal(turnStateJournalPreview))
}

func turnStateJournalResponse(status turnstate.JournalStatus) ManagementResponse {
	response := JSONResponse(http.StatusOK, status)
	response.Headers.Set("Cache-Control", "private, no-store")
	return response
}
