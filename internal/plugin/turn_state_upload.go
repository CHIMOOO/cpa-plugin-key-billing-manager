package plugin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"cpa-key-billing/internal/messages"
)

func decodeTurnStateUpload(req ManagementRequest, target any) error {
	// A 12 KiB binary chunk becomes 16 KiB of base64 plus a small JSON envelope.
	if len(req.Body) > 20<<10 {
		return messages.Errorf("Invalid settings upload chunk")
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return messages.Errorf("Invalid settings upload chunk")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return messages.Errorf("Invalid settings upload chunk")
	}
	return nil
}

func turnStateUploadError(err error) ManagementResponse {
	return jsonMessageError(http.StatusBadRequest, "invalid_turn_state_upload", messages.FromError(err))
}

func (a *App) beginTurnStateUpload(req ManagementRequest) ManagementResponse {
	var input struct {
		Size int `json:"size"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return turnStateUploadError(err)
	}
	status, err := a.turnState.BeginConfigUpload(input.Size)
	if err != nil {
		return turnStateUploadError(err)
	}
	return JSONResponse(http.StatusOK, status)
}

func (a *App) appendTurnStateUpload(req ManagementRequest) ManagementResponse {
	var input struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Data   []byte `json:"data"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return turnStateUploadError(err)
	}
	status, err := a.turnState.AppendConfigUpload(input.ID, input.Offset, input.Data)
	if err != nil {
		return turnStateUploadError(err)
	}
	return JSONResponse(http.StatusOK, status)
}

func (a *App) commitTurnStateUpload(req ManagementRequest) ManagementResponse {
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return turnStateUploadError(err)
	}
	if err := a.turnState.CommitConfigUpload(input.ID); err != nil {
		return turnStateUploadError(err)
	}
	return a.getTurnState(req)
}

func (a *App) cancelTurnStateUpload(req ManagementRequest) ManagementResponse {
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeTurnStateUpload(req, &input); err != nil {
		return turnStateUploadError(err)
	}
	a.turnState.CancelConfigUpload(input.ID)
	return JSONResponse(http.StatusOK, map[string]bool{"cancelled": true})
}
