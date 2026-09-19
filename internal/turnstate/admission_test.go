package turnstate

import (
	"testing"
	"time"
)

func TestProtectedAdmissionRechecksExpiryAfterScheduler(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["account-a"]}`)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.state.Templates[key("account-a", "model")] = Template{Account: "account-a", Model: "model", Value: tokenAt(*now), IssuedAt: *now}
	m.mu.Unlock()
	if !m.BusinessReady("account-a", "model") {
		t.Fatal("fresh template not schedulable")
	}
	*now = now.Add(time.Hour)
	if headers, _, reason := m.BeforeRequired("request", "account-a", "model", nil); reason == "" || headers != nil {
		t.Fatal("expired between scheduler and final hook was allowed")
	}
	if !m.HasProtectedAccounts() || !m.AccountProtected("account-a") {
		t.Fatal("expiry removed protection")
	}
}

func TestProtectedAdmissionUsesOneInstantForValidationAndInjection(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["account-a"]}`)); err != nil {
		t.Fatal(err)
	}
	token := tokenAt(*now)
	m.state.Templates[key("account-a", "model")] = Template{Account: "account-a", Model: "model", Value: token, IssuedAt: *now}
	instant := now.Add(time.Hour - time.Nanosecond)
	calls := 0
	m.now = func() time.Time {
		calls++
		if calls == 1 {
			return instant
		}
		return instant.Add(time.Second)
	}
	headers, _, reason := m.BeforeRequired("request", "account-a", "model", nil)
	if reason != "" || headers.Get(Header) != token {
		t.Fatal("validation and injection used different clock instants", reason, calls)
	}
}
