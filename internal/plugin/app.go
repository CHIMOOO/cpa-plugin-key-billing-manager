package plugin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/sqlite"
	"cpa-key-billing/internal/turnstate"
)

type App struct {
	store                 *billing.Store
	turnState             *turnstate.Manager
	accountRuntime        *accountRuntime
	risk                  *riskControl
	hostSchema            atomic.Uint32
	hostCaller            HostCaller
	integrationsMu        sync.Mutex
	integrationLogins     map[string]*integrationLogin
	integrationUnsaved    map[string]integrationAccount
	modelTestsMu          sync.Mutex
	modelTests            map[string]modelTestLease
	modelTestNow          func() time.Time
	admissionsMu          sync.Mutex
	admissions            map[string]*requestAdmission
	routingMu             sync.Mutex
	credentials           map[string]credentialView
	credentialsByRawID    map[string]string
	credentialRefsByIndex map[string]string
	scheduler             subsetScheduler
	pending               map[string]pendingRouteLog
	pendingSequence       uint64
}

func (a *App) SetHostCaller(caller HostCaller) {
	a.hostCaller = caller
}

func NewApp() *App {
	return newApp(billing.NewStore(openRepository, nil))
}

func newApp(store *billing.Store) *App {
	return &App{
		store:                 store,
		turnState:             turnstate.New(),
		accountRuntime:        newAccountRuntime(),
		risk:                  newRiskControl(),
		admissions:            make(map[string]*requestAdmission),
		credentials:           make(map[string]credentialView),
		credentialsByRawID:    make(map[string]string),
		credentialRefsByIndex: make(map[string]string),
		pending:               make(map[string]pendingRouteLog),
	}
}

func openRepository(path string) (billing.Repository, error) {
	return sqlite.Open(path)
}

// HandleMethod dispatches one host RPC call. A panic anywhere below is
// converted into an error envelope: the host fuses a panicking plugin, and
// taking the whole proxy down over a billing bug is not an acceptable trade.
func (a *App) HandleMethod(method string, request []byte) (response []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = fmt.Errorf("Plugin call %s panicked: %v", method, recovered)
			if a != nil && a.store != nil {
				a.store.AddPluginLog(billing.PluginLogError, "%v", err)
			}
		}
	}()
	return a.handleMethod(method, request)
}

func (a *App) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		if errConfigure := a.configure(request); errConfigure != nil {
			a.store.AddPluginLog(billing.PluginLogError, "Failed to apply plugin configuration: %v", errConfigure)
			return nil, errConfigure
		}
		return OKEnvelope(registrationForHost(a.hostSchema.Load()))
	case MethodRequestInterceptBefore:
		return a.interceptBeforeAuth(request)
	case MethodRequestInterceptAfter:
		return a.interceptAfterAuth(request)
	case MethodRequestComplete:
		return a.completeRequest(request)
	case MethodResponseInterceptAfter:
		return a.handleTurnStateResponse(request, false)
	case MethodResponseStreamChunk:
		return a.handleTurnStateResponse(request, true)
	case MethodSchedulerPick:
		return a.pickCredential(request)
	case MethodUsageHandle:
		return a.handleUsage(request)
	case MethodManagementRegister:
		return OKEnvelope(managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "Unsupported plugin method: "+method, http.StatusNotFound), nil
	}
}

func (a *App) Shutdown() {
	if a == nil || a.store == nil {
		return
	}
	a.store.Close()
}

func (a *App) configure(raw []byte) error {
	var req LifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return fmt.Errorf("Parse plugin lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errDecode := billing.DecodeConfig(req.ConfigYAML)
	if errDecode != nil {
		return errDecode
	}
	runtimePath := cfg.StateFile + ".account-runtime.json"
	runtimeSettings, errRuntime := loadAccountRuntimeSettings(runtimePath)
	if errRuntime != nil {
		return errRuntime
	}
	riskPath := cfg.StateFile + ".risk-control.json"
	riskState, errRisk := loadRiskControl(riskPath)
	if errRisk != nil {
		return errRisk
	}
	if errConfigure := a.turnState.ConfigureWith(cfg.StateFile, func() error {
		a.routingMu.Lock()
		defer a.routingMu.Unlock()
		previous := a.store.ConfigCredentials()
		if err := a.store.Configure(cfg); err != nil {
			return err
		}
		if loaded := a.store.ConfigCredentials(); !maps.Equal(previous, loaded) {
			a.replaceSyncedCredentials(previous, loaded)
		}
		return nil
	}); errConfigure != nil {
		return errConfigure
	}
	a.accountRuntime.mu.Lock()
	a.accountRuntime.path, a.accountRuntime.settings = runtimePath, runtimeSettings
	a.accountRuntime.mu.Unlock()
	a.risk.install(riskPath, riskState)
	a.hostSchema.Store(req.SchemaVersion)
	// Refresh records its result; a download failure does not disable custom prices.
	_, _ = a.store.EnsureReferencePrices()
	return nil
}

func registration() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "CHIMOOO",
			GitHubRepository: GitHubRepository,
			ConfigFields: []ConfigField{
				{
					Name:        "debug",
					Type:        "boolean",
					Description: "Record debug logs, including routing and reference price matching",
				},
				{
					Name:        "codex_fast_mode_billing",
					Type:        "boolean",
					Description: "Bill Codex priority requests at 2.5 times the standard cost",
				},
				{
					Name:        "state_file",
					Type:        "string",
					Description: "Billing database file path",
				},
			},
		},
		Capabilities: Capabilities{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
			UsagePlugin:            true,
			ManagementAPI:          true,
			Scheduler:              true,
		},
	}
}

// Schema 5 only removes per-payload history that this plugin never reads.
// Keep the original schema for older hosts, which reject a newer declaration.
func registrationForHost(hostSchema uint32) Registration {
	result := registration()
	if hostSchema >= 5 {
		result.SchemaVersion = 5
	}
	return result
}
