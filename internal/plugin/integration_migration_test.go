package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func migrationFixture(t *testing.T, app *App) (*integrationTestHost, integrationAccount, string) {
	t.Helper()
	host := &integrationTestHost{Files: map[string]integrationAccount{}}
	app.SetHostCaller(host.call)
	res := app.saveIntegration(integrationReq(t, integrationInput{Kind: "opencode-zen", Name: "Dummy", APIKey: "dummy-old", Models: []string{"glm-4.7"}}))
	if res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	var payload struct {
		Account integrationView `json:"account"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		t.Fatal(err)
	}
	account := host.Files[payload.Account.AuthFileName]
	channels, _ := integrationChannels(account)
	ref := billing.CredentialFingerprint(integrationExpectedCredentialID(account, channels[0]))
	if err := app.store.SyncConfigCredentials(map[string]billing.ConfigCredential{ref: {Provider: "dummy"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreateGroup(billing.KeyGroup{ID: "dummy-group", Name: "Dummy", Rule: billing.RouteRule{CredentialIDs: []string{ref}}}, nil); err != nil {
		t.Fatal(err)
	}
	if res := app.setAccountRuntimeSettings(integrationReq(t, accountRuntimeSettings{Accounts: map[string]accountRuntimePolicy{ref: {1}}})); res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	return host, account, ref
}

func replaceMigrationKey(t *testing.T, app *App, account integrationAccount) ManagementResponse {
	t.Helper()
	return app.saveIntegration(integrationReq(t, integrationInput{ID: account.ID, Kind: account.Kind, Name: account.Name, APIKey: "dummy-new"}))
}

func TestIntegrationReplacementPreservesGroupAndInflightPoolAcrossCommit(t *testing.T) {
	app := newConfiguredApp(t)
	host, account, old := migrationFixture(t, app)
	if !app.accountRuntime.acquire(old, "dummy-active") {
		t.Fatal("initial slot rejected")
	}
	response := replaceMigrationKey(t, app, account)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	stored := host.Files[integrationFileName(account.ID)]
	pending := stored.PendingRefMigration
	if pending == nil || !pending.Prepared || !strings.Contains(string(response.Body), `"migration_id"`) {
		t.Fatal("replacement did not prepare migration")
	}
	next := pending.Mapping[old]
	if next == "" {
		t.Fatal("missing exact replacement fingerprint")
	}
	group, _ := app.store.Group("dummy-group")
	if !slices.Contains(group.Rule.CredentialIDs, old) || !slices.Contains(group.Rule.CredentialIDs, next) {
		t.Fatal("prepare removed old grant or omitted replacement")
	}
	if app.accountRuntime.acquire(next, "dummy-parallel") {
		t.Fatal("new token bypassed old in-flight limit")
	}
	commitReq := integrationReq(t, map[string]string{"id": account.ID, "migration_id": pending.ID})
	if res := app.commitIntegrationReplacement(commitReq); res.StatusCode != 409 {
		t.Fatal("commit accepted unsynchronized host inventory")
	}
	if res := app.syncConfiguredCredentials(integrationReq(t, map[string]any{"credentials": []map[string]any{{"ref": next, "provider": "dummy"}}})); res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	for range 2 {
		if res := app.commitIntegrationReplacement(commitReq); res.StatusCode != 200 {
			t.Fatal(string(res.Body))
		}
	}
	group, _ = app.store.Group("dummy-group")
	if !slices.Equal(group.Rule.CredentialIDs, []string{next}) {
		t.Fatal("commit did not retire old grant")
	}
	app.accountRuntime.release("dummy-active")
	if !app.accountRuntime.acquire(next, "dummy-next") || app.accountRuntime.acquire(old, "dummy-alias") {
		t.Fatal("old and new no longer share concurrency pool")
	}
	if stored := host.Files[integrationFileName(account.ID)]; stored.PendingRefMigration != nil || stored.LastRefMigration != pending.ID {
		t.Fatal("completion marker missing")
	}
}

func TestIntegrationPrepareFailureDoesNotExposeChannelsAndRepairsAfterRestart(t *testing.T) {
	rawConfig := testConfigYAML(t, true)
	open := func() *App {
		app := newTestApp(t)
		if _, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: rawConfig})); err != nil {
			t.Fatal(err)
		}
		return app
	}
	app := open()
	host, account, old := migrationFixture(t, app)
	blocked := filepath.Join(t.TempDir(), "dummy-blocked")
	if err := os.WriteFile(blocked, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	app.accountRuntime.path = filepath.Join(blocked, "settings.json")
	response := replaceMigrationKey(t, app, account)
	if response.StatusCode != 502 || strings.Contains(string(response.Body), `"channels"`) || strings.Contains(string(response.Body), "dummy-new") {
		t.Fatalf("unsafe prepare failure: %s", response.Body)
	}
	stored := host.Files[integrationFileName(account.ID)]
	if stored.APIKey != "dummy-new" || stored.PendingRefMigration == nil || stored.PendingRefMigration.Prepared {
		t.Fatal("pending token was not saved before prepare")
	}
	group, _ := app.store.Group("dummy-group")
	if !slices.Contains(group.Rule.CredentialIDs, old) {
		t.Fatal("failed prepare removed old access")
	}
	app.Shutdown()
	app = open()
	t.Cleanup(app.Shutdown)
	app.SetHostCaller(host.call)
	response = app.channelIntegration(integrationReq(t, map[string]string{"id": account.ID}))
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "dummy-new") {
		t.Fatalf("restart repair: %s", response.Body)
	}
	stored = host.Files[integrationFileName(account.ID)]
	if !stored.PendingRefMigration.Prepared || app.accountRuntime.settings.Accounts[stored.PendingRefMigration.Mapping[old]].ConcurrencyLimit != 1 {
		t.Fatal("repair lost saved policy")
	}
}

func TestClineRefreshRepairsHostWriteWithoutRotatingAgain(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{Files: map[string]integrationAccount{}}
	app.SetHostCaller(host.call)
	account := integrationAccount{ID: "111111111111111111111111", Name: "Dummy", Kind: "cline-pass", APIKey: "workos:dummy-old", RefreshToken: "dummy-refresh-old", Models: []string{"cline-pass/glm-5.3"}}
	if err := app.persistIntegration(account); err != nil {
		t.Fatal(err)
	}
	rotations := 0
	host.HTTP = func(req hostHTTPRequest) (int, string) {
		if req.URL != "https://api.cline.bot/api/v1/auth/refresh" {
			t.Fatal("unexpected upstream request")
		}
		rotations++
		return 200, `{"data":{"accessToken":"dummy-rotated","refreshToken":"dummy-rotated-refresh"}}`
	}
	host.SaveError = func(integrationAccount) error { return errors.New("dummy save failure") }
	req := integrationReq(t, map[string]string{"id": account.ID})
	response := app.refreshIntegration(req)
	if response.StatusCode != 502 || rotations != 1 || strings.Contains(string(response.Body), "dummy-rotated") {
		t.Fatal("unsafe failed refresh")
	}
	if len(app.integrationUnsaved) != 1 {
		t.Fatal("rotated token unavailable for retry")
	}
	host.SaveError = nil
	for range 2 {
		response = app.refreshIntegration(req)
		if response.StatusCode != 200 || rotations != 1 {
			t.Fatalf("repair rotated upstream again: %d %s", rotations, response.Body)
		}
	}
	stored := host.Files[integrationFileName(account.ID)]
	if stored.APIKey != "workos:dummy-rotated" || stored.PendingRefMigration == nil || !stored.PendingRefMigration.Prepared || len(app.integrationUnsaved) != 0 {
		t.Fatal("refresh repair did not persist pending token")
	}
}

func TestIntegrationCommitRetriesFailedCompletionMarker(t *testing.T) {
	app := newConfiguredApp(t)
	host, account, old := migrationFixture(t, app)
	if res := replaceMigrationKey(t, app, account); res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	pending := host.Files[integrationFileName(account.ID)].PendingRefMigration
	if err := app.store.SyncConfigCredentials(map[string]billing.ConfigCredential{pending.Mapping[old]: {Provider: "dummy"}}); err != nil {
		t.Fatal(err)
	}
	host.SaveError = func(account integrationAccount) error {
		if account.PendingRefMigration == nil {
			return errors.New("dummy marker failure")
		}
		return nil
	}
	req := integrationReq(t, map[string]string{"id": account.ID, "migration_id": pending.ID})
	for range 2 {
		if res := app.commitIntegrationReplacement(req); res.StatusCode != 502 {
			t.Fatal("failed marker reported committed")
		}
	}
	host.SaveError = nil
	if res := app.commitIntegrationReplacement(req); res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	stored := host.Files[integrationFileName(account.ID)]
	if stored.PendingRefMigration != nil || stored.LastRefMigration != pending.ID {
		t.Fatal("retry did not persist marker")
	}
}

func TestClineCancelPendingAndCompletedSignIns(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{Files: map[string]integrationAccount{}}
	app.SetHostCaller(host.call)
	id := "111111111111111111111111"
	app.integrationLogins = map[string]*integrationLogin{id: {Name: "Dummy", ExpiresAt: time.Now().Add(time.Hour)}}
	req := integrationReq(t, map[string]string{"login_id": id})
	if response := app.cancelIntegrationCline(req); response.StatusCode != 200 || !strings.Contains(string(response.Body), `"cancelled"`) {
		t.Fatal(string(response.Body))
	}
	if response := app.pollIntegrationCline(req); response.StatusCode != 410 {
		t.Fatal("canceled login can still poll")
	}
	account := integrationAccount{ID: "222222222222222222222222", LoginID: id, Kind: "cline-pass", Name: "Dummy", APIKey: "workos:dummy-secret", RefreshToken: "dummy-refresh"}
	if err := app.persistIntegration(account); err != nil {
		t.Fatal(err)
	}
	response := app.cancelIntegrationCline(req)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"complete"`) || strings.Contains(string(response.Body), "dummy-secret") || strings.Contains(string(response.Body), "dummy-refresh") || strings.Contains(string(response.Body), `"api_key"`) {
		t.Fatalf("completed cancellation was unsafe: %s", response.Body)
	}
	app.SetHostCaller(func(string, any) (json.RawMessage, error) { return nil, errors.New("dummy host unavailable") })
	if response := app.cancelIntegrationCline(req); response.StatusCode != 502 {
		t.Fatal("uncertain completion reported cancelled")
	}
}

func TestClineCancelWaitsForInflightPollAndReportsCompletion(t *testing.T) {
	app := newConfiguredApp(t)
	entered, finish := make(chan struct{}), make(chan struct{})
	host := &integrationTestHost{Files: map[string]integrationAccount{}, HTTP: func(req hostHTTPRequest) (int, string) {
		switch req.URL {
		case "https://api.workos.com/user_management/authenticate":
			close(entered)
			<-finish
			return 200, `{"access_token":"dummy-access","refresh_token":"dummy-refresh"}`
		case "https://api.cline.bot/api/v1/auth/register":
			return 200, `{"data":{"accessToken":"dummy-final","refreshToken":"dummy-refresh-final"}}`
		case "https://api.cline.bot/api/v1/models":
			return 200, `{"data":[{"id":"cline-pass/glm-5.3"}]}`
		default:
			t.Fatal("unexpected request")
			return 500, ""
		}
	}}
	app.SetHostCaller(host.call)
	id := "111111111111111111111111"
	app.integrationLogins = map[string]*integrationLogin{id: {Name: "Dummy", ExpiresAt: time.Now().Add(time.Hour), Interval: 5}}
	req := integrationReq(t, map[string]string{"login_id": id})
	pollDone, cancelDone := make(chan ManagementResponse, 1), make(chan ManagementResponse, 1)
	go func() { pollDone <- app.pollIntegrationCline(req) }()
	<-entered
	go func() { cancelDone <- app.cancelIntegrationCline(req) }()
	close(finish)
	if res := <-pollDone; res.StatusCode != 200 {
		t.Fatal(string(res.Body))
	}
	if res := <-cancelDone; res.StatusCode != 200 || !strings.Contains(string(res.Body), `"complete"`) {
		t.Fatal("cancel did not report completed in-flight poll")
	}
}
