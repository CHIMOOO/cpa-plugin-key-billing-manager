package turnstate

import (
	"net/http"
	"testing"
)

func TestTemplateCookiesAreHarvestedAndInjected(t *testing.T) {
	headers := http.Header{"Set-Cookie": {"__cf_bm=abc; Path=/; HttpOnly", "_cfuvid=xyz; Path=/", "gone=; Max-Age=0"}}
	if got := responseCookies(headers); got != "__cf_bm=abc; _cfuvid=xyz" {
		t.Fatalf("harvested cookies = %q", got)
	}
	template := Template{Cookies: "__cf_bm=abc; _cfuvid=xyz"}
	request := http.Header{"cookie": {"client=1; __cf_bm=old"}}
	if got := injectedCookies(request, template, Config{InjectCookies: true}); got != "client=1; __cf_bm=abc; _cfuvid=xyz" {
		t.Fatalf("injected cookies = %q", got)
	}
	if got := injectedCookies(request, template, Config{}); got != "" {
		t.Fatalf("disabled injection = %q", got)
	}
	if cookieCount(template.Cookies) != 2 {
		t.Fatal("cookie count")
	}
}

func TestMigrateDefaultsMovesOnlyTheOldLifetimeOnce(t *testing.T) {
	previousTTL, previousLead := shippedTTLSeconds, shippedRenewBeforeMinutes
	shippedTTLSeconds, shippedRenewBeforeMinutes = 240, 2
	defer func() { shippedTTLSeconds, shippedRenewBeforeMinutes = previousTTL, previousLead }()
	state := diskState{Config: DefaultConfig()}
	if !migrateDefaults(&state) || state.Config.TTLSeconds != 240 || state.Config.RenewBeforeMinutes != 2 {
		t.Fatalf("old default not migrated: %+v", state.Config)
	}
	state.Config.TTLSeconds, state.Config.RenewBeforeMinutes = 3600, 0
	if migrateDefaults(&state) || state.Config.TTLSeconds != 3600 {
		t.Fatal("a later operator choice was migrated again")
	}
	custom := diskState{Config: DefaultConfig()}
	custom.Config.TTLSeconds = 600
	if migrateDefaults(&custom) || custom.Config.TTLSeconds != 600 {
		t.Fatal("a custom lifetime was migrated")
	}
	if err := validateConfig(&state.Config); err != nil {
		t.Fatal(err)
	}
}
