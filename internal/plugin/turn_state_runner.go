package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

const turnStateRunnerLease = 90 * time.Second

type turnStateRunnerControl struct {
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	Revision uint64 `json:"revision"`
}

type turnStateRunnerEvent struct {
	Sequence uint64                `json:"sequence"`
	At       time.Time             `json:"at"`
	Result   turnstate.ProbeResult `json:"result"`
}

type turnStateRunnerStatus struct {
	Enabled          bool                   `json:"enabled"`
	Revision         uint64                 `json:"revision"`
	Epoch            string                 `json:"epoch"`
	Phase            string                 `json:"phase"`
	Online           bool                   `json:"online"`
	InstanceID       string                 `json:"instance_id,omitempty"`
	Version          string                 `json:"version,omitempty"`
	LastSeenAt       time.Time              `json:"last_seen_at"`
	LeaseExpiresAt   time.Time              `json:"lease_expires_at"`
	NextCheckAt      time.Time              `json:"next_check_at"`
	InFlight         bool                   `json:"in_flight"`
	Events           []turnStateRunnerEvent `json:"events"`
	LastError        string                 `json:"last_error,omitempty"`
	LastErrorMessage messages.Message       `json:"last_error_message,omitzero"`
	NextTickSeconds  int                    `json:"next_tick_seconds"`
}

// The independent collector process supplies periodic management calls. This
// object performs only synchronous work within those calls and owns no timer
// or goroutine. Its gate pins the configuration path while a probe runs; Stop
// uses the independent control mutex and can persist while that probe drains.
type turnStateRunner struct {
	gate         sync.RWMutex
	controlMu    sync.Mutex
	mu           sync.Mutex
	path         string
	control      turnStateRunnerControl
	epoch        string
	instance     string
	version      string
	lastSeen     time.Time
	leaseUntil   time.Time
	nextCheck    time.Time
	configToken  string
	inFlight     bool
	manual       bool
	events       []turnStateRunnerEvent
	sequence     uint64
	lastError    string
	stopPending  bool
	now          func() time.Time
	writeControl func(string, any) error
	probe        func() ManagementResponse // Tests supply a bounded fake upstream.
}

func newTurnStateRunner() *turnStateRunner {
	return &turnStateRunner{control: turnStateRunnerControl{Version: 1}, epoch: newRunnerEpoch(), now: time.Now}
}

func newRunnerEpoch() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	// Epoch is only a UI event-identity boundary, never an authorization token.
	return time.Now().UTC().Format("20060102T150405.000000000")
}

// loadConfiguration is called with gate held exclusively. A changed path is
// validated before the billing/State stores commit, so malformed control data
// cannot leave those stores pointing at different installations.
func (r *turnStateRunner) loadConfiguration(path string) (turnStateRunnerControl, bool, error) {
	r.mu.Lock()
	if r.path == path {
		control := r.control
		r.mu.Unlock()
		return control, false, nil
	}
	r.mu.Unlock()
	control := turnStateRunnerControl{Version: 1}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return control, true, nil
	}
	if err != nil {
		return control, false, errors.New("Cannot read server collection settings")
	}
	var stored struct {
		Version  int     `json:"version"`
		Enabled  *bool   `json:"enabled"`
		Revision *uint64 `json:"revision"`
	}
	if len(raw) > 4096 || decodeStrict(raw, &stored) != nil || stored.Version != 1 || stored.Enabled == nil || stored.Revision == nil || *stored.Revision > 1<<53-1 {
		return control, false, errors.New("Invalid server collection settings")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return control, false, errors.New("Cannot secure server collection settings")
	}
	control.Enabled, control.Revision = *stored.Enabled, *stored.Revision
	return control, true, nil
}

func (r *turnStateRunner) installConfiguration(path string, control turnStateRunnerControl, changed bool) {
	if !changed {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path, r.control, r.epoch = path, control, newRunnerEpoch()
	r.instance, r.version, r.configToken, r.lastError = "", "", "", ""
	r.lastSeen, r.leaseUntil, r.nextCheck = time.Time{}, time.Time{}, time.Time{}
	r.inFlight, r.manual, r.sequence, r.events = false, false, 0, nil
	r.stopPending = false
}

func (r *turnStateRunner) statusLocked() turnStateRunnerStatus {
	now := r.now()
	online := r.instance != "" && r.leaseUntil.After(now)
	phase := "stopped"
	if r.inFlight {
		phase = "running"
		if !r.control.Enabled || r.stopPending || r.manual {
			phase = "draining"
		}
	} else if r.control.Enabled {
		phase = "waiting"
		if !online {
			phase = "offline"
		}
	}
	// Keep idle heartbeats bounded, but do not add a fixed five-second gap to
	// every ready bucket. Probe results already enforce the account cooldown;
	// respecting their shorter hint leaves more time for early renewals.
	nextTick := 5
	if r.control.Enabled && !r.inFlight && r.nextCheck.After(now) {
		wait := r.nextCheck.Sub(now)
		if wait < 5*time.Second {
			nextTick = max(1, int((wait+time.Second-1)/time.Second))
		}
	}
	return turnStateRunnerStatus{Enabled: r.control.Enabled, Revision: r.control.Revision, Epoch: r.epoch,
		Phase: phase, Online: online, InstanceID: r.instance, Version: r.version, LastSeenAt: r.lastSeen,
		LeaseExpiresAt: r.leaseUntil, NextCheckAt: r.nextCheck, InFlight: r.inFlight,
		Events: append([]turnStateRunnerEvent{}, r.events...), LastError: r.lastError,
		LastErrorMessage: messages.Literal(r.lastError), NextTickSeconds: nextTick}
}

func (r *turnStateRunner) status() turnStateRunnerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}

func runnerResponse(status int, value any) ManagementResponse {
	response := JSONResponse(status, value)
	response.Headers.Set("Cache-Control", "private, no-store")
	return response
}

func (a *App) getTurnStateRunner(ManagementRequest) ManagementResponse {
	return runnerResponse(http.StatusOK, a.turnStateRunner.status())
}

func (a *App) setTurnStateRunner(req ManagementRequest) ManagementResponse {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if len(req.Body) > 1024 || decodeStrict(req.Body, &input) != nil || input.Enabled == nil {
		return JSONError(http.StatusBadRequest, "invalid_runner_settings", "Specify whether server collection is enabled")
	}
	r := a.turnStateRunner
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	if *input.Enabled {
		config := a.turnState.Status().Config
		if len(config.ProbeAccounts) == 0 || len(config.Models) == 0 {
			return JSONError(http.StatusBadRequest, "invalid_runner_scope", "Save the probe accounts and models first")
		}
	}
	r.mu.Lock()
	if r.control.Enabled == *input.Enabled {
		status := r.statusLocked()
		r.mu.Unlock()
		return runnerResponse(http.StatusOK, status)
	}
	next, path, write := r.control, r.path, r.writeControl
	next.Enabled, next.Revision = *input.Enabled, next.Revision+1
	// While a durable Stop is being written, new ticks must not reserve one
	// more request. A failed save removes only this temporary admission barrier.
	r.stopPending = !*input.Enabled
	r.mu.Unlock()
	if write == nil {
		write = writePrivateJSON
	}
	if err := write(path, next); err != nil {
		r.mu.Lock()
		r.stopPending = false
		r.mu.Unlock()
		return JSONError(http.StatusInternalServerError, "runner_save_failed", "Cannot save server collection settings")
	}
	r.mu.Lock()
	r.control = next
	r.stopPending = false
	r.nextCheck = time.Time{}
	r.lastError = ""
	status := r.statusLocked()
	r.mu.Unlock()
	return runnerResponse(http.StatusOK, status)
}

var runnerIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)

func (a *App) tickTurnStateRunner(req ManagementRequest) (out ManagementResponse) {
	var input struct {
		InstanceID string `json:"instance_id"`
		Version    string `json:"version"`
	}
	if len(req.Body) > 2048 || decodeStrict(req.Body, &input) != nil || !runnerIdentityPattern.MatchString(input.InstanceID) || !runnerIdentityPattern.MatchString(input.Version) || len(input.Version) > 64 {
		return JSONError(http.StatusBadRequest, "invalid_runner_identity", "Invalid server collector identity")
	}
	r := a.turnStateRunner
	r.gate.RLock()
	defer r.gate.RUnlock()
	// This independent management heartbeat may synchronize response caches;
	// no user request or response callback waits for this disk write.
	_ = a.turnState.PersistLearned()
	state := a.turnState.Status()
	r.mu.Lock()
	now := r.now()
	if r.instance != "" && r.instance != input.InstanceID && (r.leaseUntil.After(now) || r.inFlight) {
		r.mu.Unlock()
		return JSONError(http.StatusConflict, "runner_owned", "Another server collector owns the active lease")
	}
	r.instance, r.version, r.lastSeen, r.leaseUntil = input.InstanceID, input.Version, now, now.Add(turnStateRunnerLease)
	if r.inFlight {
		r.mu.Unlock()
		return JSONError(http.StatusConflict, "runner_busy", "A server collection probe is already running")
	}
	if r.configToken != state.ProxyConfigRevision {
		r.configToken, r.nextCheck = state.ProxyConfigRevision, time.Time{}
	}
	if !r.control.Enabled || r.stopPending || r.nextCheck.After(now) {
		status := r.statusLocked()
		r.mu.Unlock()
		return runnerResponse(http.StatusOK, status)
	}
	r.inFlight = true
	r.manual = false
	probe := r.probe
	startRevision := r.control.Revision
	r.mu.Unlock()
	// Finalize even if a host callback panics; a failed tick must not leave a
	// permanent active flag and silently disable a durable collection intent.
	result := turnstate.ProbeResult{Action: "error", Reason: "Server collection could not complete this probe; it will retry"}
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.inFlight = false
		finished := r.now()
		result = sanitizeRunnerResult(result)
		r.nextCheck = finished.Add(30 * time.Second)
		if !result.NextCheckAt.IsZero() {
			r.nextCheck = result.NextCheckAt
			if r.nextCheck.Before(finished.Add(2 * time.Second)) {
				r.nextCheck = finished.Add(2 * time.Second)
			}
			if r.nextCheck.After(finished.Add(time.Minute)) {
				r.nextCheck = finished.Add(time.Minute)
			}
		}
		if r.control.Revision != startRevision {
			// An explicit Stop/Start during the draining request supersedes
			// that old request's scheduling hint, but does not spawn a second.
			r.nextCheck = time.Time{}
		}
		r.lastError = ""
		if result.Action == "error" {
			r.lastError = result.Reason
		}
		r.sequence++
		r.events = append(r.events, turnStateRunnerEvent{Sequence: r.sequence, At: finished, Result: result})
		if len(r.events) > 100 {
			r.events = append([]turnStateRunnerEvent(nil), r.events[len(r.events)-100:]...)
		}
		out = runnerResponse(http.StatusOK, r.statusLocked())
	}()
	if probe == nil {
		probe = func() ManagementResponse { return a.executeTurnStateProbe(ManagementRequest{Body: []byte(`{}`)}) }
	}
	response := probe()
	if response.StatusCode == http.StatusOK {
		var observed turnstate.ProbeResult
		if json.Unmarshal(response.Body, &observed) == nil && observed.Action != "" {
			result = observed
		}
	}
	return ManagementResponse{}
}

func sanitizeRunnerResult(result turnstate.ProbeResult) turnstate.ProbeResult {
	if result.Exit != "" && result.Exit != "direct" {
		proxy, err := url.Parse(result.Exit)
		if err != nil || proxy.Hostname() == "" {
			result.Exit = "invalid proxy"
		} else {
			result.Exit = proxy.Scheme + "://" + proxy.Host
			if proxy.User != nil {
				result.Exit = proxy.Scheme + "://***@" + proxy.Host
			}
		}
	}
	result.Account = strings.TrimSpace(result.Account)
	if result.ReasonMessage.IsZero() {
		result.ReasonMessage = messages.Literal(result.Reason)
	}
	return result
}

// Manual probes remain available only while server collection is stopped.
// Holding a shared gate keeps a configuration path switch from overtaking a
// manual probe without making a Stop wait for an in-flight server request.
func (a *App) beginManualTurnStateProbe() (func(), bool) {
	r := a.turnStateRunner
	r.gate.RLock()
	r.mu.Lock()
	if r.control.Enabled || r.inFlight {
		r.mu.Unlock()
		r.gate.RUnlock()
		return nil, false
	}
	r.inFlight, r.manual = true, true
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.inFlight, r.manual = false, false
		r.mu.Unlock()
		r.gate.RUnlock()
	}, true
}
