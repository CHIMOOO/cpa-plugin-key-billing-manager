package turnstate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAccountRefusalRecoversAfterTenMinutesWithoutClearing(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"],"models":["model-a","model-b"],"probe_proxies":["http://first.invalid:8080","http://second.invalid:8080"]}`)); err != nil {
				t.Fatal(err)
			}
			// A failed early renewal must preserve the old template and deadline.
			issued := now.Add(-56 * time.Minute)
			learn(t, m, tokenAt(issued))
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: status}, nil
			}
			started := *now
			result, err := m.Probe("account-a", "model-a", dummyCredential)
			if err != nil || result.Action != "error" || result.Status != status || !result.NextCheckAt.Equal(started.Add(2*time.Second)) {
				t.Fatalf("refusal blocked scheduling of other accounts: %+v %v", result, err)
			}
			if status != 429 && !strings.Contains(result.Reason, fmt.Sprintf("HTTP %d", status)) {
				t.Fatal("refusal did not identify its HTTP status", result)
			}
			if got := m.state.Cooldowns[accountKey("account-a")].Until; !got.Equal(started.Add(10 * time.Minute)) {
				t.Fatalf("account pause = %v", got)
			}
			// Restart must retain both account protection and the pending exit cursor.
			m = readRestarted(t, m)
			calls := 0
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				calls++
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			*now = started.Add(3 * time.Second)
			for _, model := range []string{"model-a", "model-b"} {
				result, err := m.Probe("account-a", model, dummyCredential)
				if err != nil || result.Action != "account_wait" || !result.NextCheckAt.Equal(started.Add(10*time.Minute)) || calls != 0 {
					t.Fatalf("paused account used another model/exit: %+v %v", result, err)
				}
			}
			if template := m.state.Templates[key("account-a", "model-a")]; !template.IssuedAt.Equal(issued) {
				t.Fatal("account pause changed the existing template")
			}
			if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Action != "harvested" || calls != 1 {
				t.Fatal("one paused account blocked another", result, err)
			}
			*now = started.Add(10*time.Minute - time.Nanosecond)
			if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "account_wait" || calls != 1 {
				t.Fatal("account retry ignored the pause", result, err)
			}
			*now = started.Add(10 * time.Minute)
			result, err = m.Probe("account-a", "model-a", dummyCredential)
			if err != nil || result.Action != "harvested" || calls != 2 || result.Exit != "http://second.invalid:8080" {
				t.Fatalf("account did not resume on an untried exit: %+v %v", result, err)
			}
			if until := m.state.Cooldowns[proxyKey("account-a", "model-a", "http://first.invalid:8080", false)].Until; !until.Equal(started.Add(55 * time.Minute)) {
				t.Fatal("account recovery discarded the static exit cooldown", until)
			}
		})
	}
}

func TestProbeWaitReasonsDistinguishAccountsAndExits(t *testing.T) {
	for _, scope := range []string{"account", "exit", "mixed", "fresh"} {
		t.Run(scope, func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"]}`)); err != nil {
				t.Fatal(err)
			}
			for _, account := range []string{"account-a", "account-b"} {
				id := accountKey(account)
				if scope == "exit" || scope == "mixed" && account == "account-b" {
					id = proxyKey(account, "model-a", "", false)
				}
				m.state.Cooldowns[id] = cooldown{Until: now.Add(10 * time.Minute)}
				if scope == "fresh" {
					m.state.Templates[key(account, "model-a")] = Template{Account: account, Model: "model-a", Value: tokenAt(*now), IssuedAt: *now}
				}
			}
			result, err := m.Probe("", "", func(string) (Credential, error) {
				t.Fatal("wait-only selection fetched a credential")
				return Credential{}, nil
			})
			wantAction, wantReason := "cooling", "All exits"
			switch scope {
			case "account":
				wantAction, wantReason = "account_wait", "waiting for account pauses;"
			case "mixed":
				wantReason = "account pauses or exit cooldowns"
			case "fresh":
				wantAction, wantReason = "fresh", "All templates are fresh"
			}
			if err != nil || result.Action != wantAction || !strings.Contains(result.Reason, wantReason) {
				t.Fatalf("incorrect wait diagnosis (%s): %+v %v", scope, result, err)
			}
			wantNext := now.Add(10 * time.Minute)
			if scope == "fresh" {
				wantNext = now.Add(55 * time.Minute)
			}
			if !result.NextCheckAt.Equal(wantNext) {
				t.Fatalf("wrong earliest recheck: %+v, want %v", result, wantNext)
			}
		})
	}
}

func TestExitBlocked403MovesToAnotherExitWithoutPausingTheAccount(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["account-a"],"models":["model-a"],"probe_proxies":["http://first.invalid:8080","http://second.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 403, ExitBlocked: true}, nil
	}
	started := *now
	result, err := m.Probe("account-a", "model-a", dummyCredential)
	if err != nil || result.Action != "error" || !strings.Contains(result.Reason, "another exit") {
		t.Fatalf("blocked exit result = %+v %v", result, err)
	}
	if got := m.state.Cooldowns[accountKey("account-a")].Until; got.After(started.Add(2 * time.Second)) {
		t.Fatalf("a blocked exit paused the account until %v", got)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	*now = started.Add(2 * time.Second)
	result, err = m.Probe("account-a", "model-a", dummyCredential)
	if err != nil || result.Action != "harvested" || result.Exit != "http://second.invalid:8080" {
		t.Fatalf("the account did not continue on another exit: %+v %v", result, err)
	}
}
