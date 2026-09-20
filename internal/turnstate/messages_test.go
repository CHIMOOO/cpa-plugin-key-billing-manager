package turnstate

import (
	"testing"

	"cpa-key-billing/internal/messages"
)

func TestProbeReasonsPreserveTranslationMetadata(t *testing.T) {
	for _, tc := range []struct {
		status int
		key    string
	}{
		{200, "backend.turn_state_harvested"},
		{401, "backend.turn_state_account_paused"},
		{429, "backend.turn_state_rate_limited"},
		{503, "backend.turn_state_upstream_status"},
	} {
		m, now := newTestManager(t)
		m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
			return ProbeResponse{Status: tc.status, Value: tokenAt(*now)}, nil
		}
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.ReasonMessage.Key != tc.key || result.ReasonMessage.Text != result.Reason {
			t.Fatalf("HTTP %d reason lost translation metadata: %+v, %v", tc.status, result, err)
		}
		if tc.status == 503 && result.ReasonMessage.Params["v0"] != "503" {
			t.Fatalf("HTTP status was not preserved as a translation parameter: %+v", result.ReasonMessage)
		}
	}
}

func TestTurnStateValidationPreservesTranslationMetadata(t *testing.T) {
	m, _ := newTestManager(t)
	if detail := messages.FromError(m.Update([]byte(`{"inject_mode":"invalid"}`))); detail.Key != "backend.turn_state_invalid_inject_mode" {
		t.Fatalf("validation lost its translation key: %+v", detail)
	}
	_, err := cleanList([]string{"model-a", "model-b"}, 1)
	detail := messages.FromError(err)
	if detail.Key != "backend.turn_state_list_limit" || detail.Params["v0"] != "1" {
		t.Fatalf("validation lost its numeric translation parameter: %+v", detail)
	}
}
