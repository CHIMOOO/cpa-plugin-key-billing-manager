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
	"sync/atomic"
	"time"

	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

const turnStateRunnerLease = 90 * time.Second

// Lanes start new probes only this long into a tick. One more probe's 25 s
// transport limit still ends the tick before the collector's 45 s timeout.
const turnStateLaneWindow = 12 * time.Second

// An idle lane waits at most this long for a paced or busy bucket, and only
// while another lane's probe keeps the tick open anyway.
const turnStateLaneWait = 3 * time.Second

// Complete proxy URLs can expand during JSON encoding. Bound event bytes as
// well as count so old collectors' 2 MiB response limit remains sufficient.
const turnStateRunnerEventBytes = 1 << 20

type turnStateRunnerControl struct {
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	Revision uint64 `json:"revision"`
}

type turnStateRunnerEvent struct {
	Sequence  uint64                `json:"sequence"`
	At        time.Time             `json:"at"`
	Result    turnstate.ProbeResult `json:"result"`
	jsonBytes int
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
	wakeRevision uint64
	configToken  string
	inFlight     bool
	manual       bool
	events       []turnStateRunnerEvent
	eventBytes   int
	sequence     uint64
	lastError    string
	stopPending  bool
	now          func() time.Time
	writeControl func(string, any) error
	probe        func() ManagementResponse // Tests supply a bounded fake upstream.
	// Tests replace one lane probe and shorten the lane timing.
	probeLimit func(limit int) ManagementResponse
	laneWindow time.Duration
	laneWait   time.Duration
	sleep      func(time.Duration)
}

func newTurnStateRunner() *turnStateRunner {
	return &turnStateRunner{control: turnStateRunnerControl{Version: 1}, epoch: newRunnerEpoch(), now: time.Now,
		laneWindow: turnStateLaneWindow, laneWait: turnStateLaneWait, sleep: time.Sleep}
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
	r.eventBytes = 0
	r.wakeRevision = 0
	r.stopPending = false
}

// requestCheck invalidates a cached due-time hint without changing durable
// collection intent or starting a probe. The independent collector will
// re-evaluate normal template, cooldown, and budget rules on its next heartbeat.
func (r *turnStateRunner) requestCheck() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakeRevision++
	r.nextCheck = time.Time{}
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
	if *input.Enabled && !a.turnState.Active() {
		return turnStateSuspendedError()
	}
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
	// A suspended State keeps the durable start intent but sends nothing.
	if !r.control.Enabled || r.stopPending || state.Config.Suspended || r.nextCheck.After(now) {
		status := r.statusLocked()
		r.mu.Unlock()
		return runnerResponse(http.StatusOK, status)
	}
	r.inFlight = true
	r.manual = false
	probe, probeLimit := r.probe, r.probeLimit
	window, laneWait, sleep := r.laneWindow, r.laneWait, r.sleep
	startRevision := r.control.Revision
	startWakeRevision := r.wakeRevision
	r.mu.Unlock()
	// Lanes log real probes as they finish. The finalizer logs the rest: the
	// test hook's result, or one idle result when no lane ran a probe. r.mu
	// guards everything the lanes share with the finalizer.
	var results []turnstate.ProbeResult
	var fallback turnstate.ProbeResult
	var hint time.Time
	recorded := 0
	// Finalize even if a host callback panics; a failed tick must not leave a
	// permanent active flag and silently disable a durable collection intent.
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.inFlight = false
		finished := r.now()
		if recorded == 0 && fallback.Action != "" {
			results = append(results, fallback)
		}
		if len(results) == 0 && recorded == 0 {
			results = []turnstate.ProbeResult{{Action: "error", Reason: "Server collection could not complete this probe; it will retry"}}
		}
		// The earliest hint among parallel probes decides the next check.
		for i := range results {
			results[i] = sanitizeRunnerResult(results[i])
			hint = earlierRunnerHint(hint, results[i].NextCheckAt)
		}
		r.nextCheck = runnerCheckAt(finished, hint)
		if r.control.Revision != startRevision || r.wakeRevision != startWakeRevision {
			// Stop/Start or cleared cooldowns supersede an in-flight request's
			// scheduling hint, without admitting a second concurrent request.
			r.nextCheck = time.Time{}
		}
		if recorded == 0 {
			r.lastError = ""
		}
		for _, result := range results {
			// Management logs describe the next scheduler check, rather than the
			// manager's potentially much later account/exit cooldown deadline.
			result.NextCheckAt = r.nextCheck
			r.appendEventLocked(finished, result)
		}
		out = runnerResponse(http.StatusOK, r.statusLocked())
	}()
	if probe != nil {
		// The test hook keeps one call per tick.
		response := probe()
		var result turnstate.ProbeResult
		if response.StatusCode == http.StatusOK && json.Unmarshal(response.Body, &result) == nil && result.Action != "" {
			results = append(results, result)
		}
		return ManagementResponse{}
	}
	// Each lane keeps its slot busy: when its probe finishes it logs the
	// result and asks for the next due bucket without waiting for slower
	// lanes. The manager runs one probe per account at a time (berserk renewal
	// excepted), so parallel lanes serve different accounts.
	slots := a.turnState.ProbeConcurrency()
	if probeLimit == nil {
		probeLimit = func(limit int) ManagementResponse {
			return a.executeTurnStateProbeLimit(ManagementRequest{Body: []byte(`{}`)}, limit)
		}
	}
	// The window is real time, independent of the scheduling clock. running
	// counts lanes inside a probe call; a call that finds nothing returns at once.
	started := time.Now()
	var running atomic.Int32
	var panicked any
	admit := func() bool {
		r.mu.Lock()
		open := r.control.Enabled && !r.stopPending && r.control.Revision == startRevision && panicked == nil &&
			time.Since(started) < window
		r.mu.Unlock()
		return open && a.turnState.Active()
	}
	call := func() ManagementResponse {
		running.Add(1)
		defer running.Add(-1)
		return probeLimit(slots)
	}
	var wg sync.WaitGroup
	for range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Re-raise below on this goroutine, so the App recovery boundary and
			// the deferred finalizer still see a callback panic.
			defer func() {
				if value := recover(); value != nil {
					r.mu.Lock()
					if panicked == nil {
						panicked = value
					}
					r.mu.Unlock()
				}
			}()
			for admit() {
				response := call()
				var result turnstate.ProbeResult
				if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &result) != nil || result.Action == "" {
					return
				}
				r.mu.Lock()
				now := r.now()
				if result.Account != "" {
					// Log a real probe with its own completion time. Its event shows
					// the check this result asks for; the tick's is not known yet.
					if recorded == 0 {
						r.lastError = ""
					}
					recorded++
					hint = earlierRunnerHint(hint, result.NextCheckAt)
					result = sanitizeRunnerResult(result)
					result.NextCheckAt = runnerCheckAt(now, result.NextCheckAt)
					r.appendEventLocked(now, result)
					r.mu.Unlock()
					continue
				}
				// Nothing was due for this lane. Lanes that found nothing are not
				// separate log entries; the first such result is the fallback.
				if fallback.Action == "" {
					fallback = result
					hint = earlierRunnerHint(hint, result.NextCheckAt)
				}
				r.mu.Unlock()
				wait := result.NextCheckAt.Sub(now)
				if (result.Action != "cooling" && result.Action != "account_wait") || wait <= 0 || wait > laneWait ||
					running.Load() == 0 || time.Since(started)+wait >= window {
					return
				}
				sleep(wait)
			}
		}()
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return ManagementResponse{}
}

// appendEventLocked logs one result and keeps the newest events within both
// the count and the byte limit.
func (r *turnStateRunner) appendEventLocked(at time.Time, result turnstate.ProbeResult) {
	if result.Action == "error" {
		r.lastError = result.Reason
	}
	r.sequence++
	event := turnStateRunnerEvent{Sequence: r.sequence, At: at, Result: result}
	encoded, _ := json.Marshal(event)
	event.jsonBytes = len(encoded) + 1 // Include the array separator.
	r.events = append(r.events, event)
	r.eventBytes += event.jsonBytes
	removed := 0
	for len(r.events)-removed > 1 && (len(r.events)-removed > 100 || r.eventBytes > turnStateRunnerEventBytes) {
		r.eventBytes -= r.events[removed].jsonBytes
		removed++
	}
	if removed > 0 {
		r.events = append([]turnStateRunnerEvent(nil), r.events[removed:]...)
	}
}

func earlierRunnerHint(current, at time.Time) time.Time {
	if !at.IsZero() && (current.IsZero() || at.Before(current)) {
		return at
	}
	return current
}

// runnerCheckAt bounds a scheduling hint to the heartbeat range: never before
// the ordinary 2 s request spacing, never after a minute, 30 s without a hint.
func runnerCheckAt(finished, hint time.Time) time.Time {
	if hint.IsZero() {
		return finished.Add(30 * time.Second)
	}
	if hint.Before(finished.Add(2 * time.Second)) {
		return finished.Add(2 * time.Second)
	}
	if hint.After(finished.Add(time.Minute)) {
		return finished.Add(time.Minute)
	}
	return hint
}

func sanitizeRunnerResult(result turnstate.ProbeResult) turnstate.ProbeResult {
	if result.Exit != "" && result.Exit != "direct" {
		proxy, err := url.Parse(result.Exit)
		if err != nil || proxy.Hostname() == "" ||
			(proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") ||
			strings.ContainsAny(result.Exit, "\r\n\x00") {
			result.Exit = "invalid proxy"
		}
	}
	// Recent events are returned only through authenticated, no-store management
	// responses. Preserve the configured URL so session-specific proxies sharing
	// a gateway remain distinguishable; collector stdout never logs these events.
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
