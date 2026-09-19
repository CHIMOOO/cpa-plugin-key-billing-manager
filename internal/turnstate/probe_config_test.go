package turnstate

import (
	"testing"
	"time"
)

func TestProbeFinishesBeforeTemplateConfigurationChanges(t *testing.T) {
	m, now := newTestManager(t)
	started, finish := make(chan struct{}), make(chan struct{})
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		close(started)
		<-finish
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	type probeOutcome struct {
		result ProbeResult
		err    error
	}
	probed := make(chan probeOutcome, 1)
	go func() {
		result, err := m.Probe("", "", dummyCredential)
		probed <- probeOutcome{result: result, err: err}
	}()
	<-started
	updating := make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		close(updating)
		updated <- m.Update([]byte(`{"template_length":316}`))
	}()
	<-updating
	select {
	case err := <-updated:
		close(finish)
		<-probed
		t.Fatalf("configuration overtook in-flight probe: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	outcome := <-probed
	if outcome.err != nil || outcome.result.Action != "harvested" {
		t.Fatalf("probe evaluated against the wrong configuration: %+v, %v", outcome.result, outcome.err)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.Config.TemplateLength != 316 || len(status.Templates) != 0 || status.Counters.Learned != 1 {
		t.Fatalf("template configuration did not apply after the completed probe: %+v", status)
	}
}

func TestStaticProbeRenewsBeforeShortTTLExpires(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"ttl_seconds":600}`)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		calls++
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatalf("initial probe = %+v, %v", result, err)
	}
	*now = now.Add(450 * time.Second)
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "harvested" || calls != 2 {
		t.Fatalf("successful static exit could not renew a short-TTL template: %+v, calls=%d, err=%v", result, calls, err)
	}
}

func TestStaticProbeRenewalUsesTokenIssueTime(t *testing.T) {
	m, now := newTestManager(t)
	calls := 0
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		calls++
		issued := *now
		if calls == 1 {
			issued = issued.Add(-50 * time.Minute)
		}
		return ProbeResponse{Status: 200, Value: tokenAt(issued)}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatalf("initial probe = %+v, %v", result, err)
	}
	*now = now.Add(5 * time.Minute)
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "harvested" || calls != 2 {
		t.Fatalf("successful static exit ignored the token's original issue time: %+v, calls=%d, err=%v", result, calls, err)
	}
}
