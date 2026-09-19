package billing

import (
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"
)

const referencePriceCacheCapacity = 1024
const referencePriceOperationTimeout = 40 * time.Second

type referencePriceStorageError struct{ error }

// ReferencePrice preserves one provider/model entry from models.dev. Nil rates
// mean its cost is absent or cannot be represented by the supported token rates.
type ReferencePrice struct {
	ProviderID  string `json:"provider_id"`
	ModelID     string `json:"model_id"`
	IsCanonical bool   `json:"is_canonical"`
	*PriceRates
}

func (price ReferencePrice) Validate() error {
	if strings.TrimSpace(price.ProviderID) == "" || strings.TrimSpace(price.ModelID) == "" {
		return invalidf("Reference price provider ID and model ID are required")
	}
	if price.PriceRates == nil {
		return nil
	}
	return price.PriceRates.validate(price.ModelID)
}

type ReferencePriceMetadata struct {
	SourceURL           string    `json:"source_url"`
	ContentHash         string    `json:"content_hash"`
	Version             uint64    `json:"version"`
	FetchedAt           time.Time `json:"fetched_at,omitzero"`
	ModelCount          int       `json:"model_count"`
	Usable              bool      `json:"usable"`
	LastAttemptAt       time.Time `json:"last_attempt_at,omitzero"`
	RetryAfter          time.Time `json:"retry_after,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Refreshing          bool      `json:"refreshing"`
}

type ReferencePriceRepository interface {
	LoadReferencePriceMetadata(context.Context) (ReferencePriceMetadata, error)
	SaveReferencePrices(context.Context, ReferencePriceMetadata, []ReferencePrice) error
	ReferencePriceCandidates(context.Context, []string) ([]ReferencePrice, error)
	SearchReferencePrices(context.Context, string, int) ([]ReferencePrice, error)
	Close() error
}

type referencePriceMatch struct {
	price           ReferencePrice
	found           bool
	metadata        ReferencePriceMetadata
	refreshSequence uint64
}
type referencePriceCacheEntry struct {
	key   string
	price ReferencePrice
	found bool
}

// Each Store owns a repository and bounded match entries. storageMu protects SQL reads,
// publication and Close, never network/parse work. mu protects small memory state.
type referencePriceManager struct {
	download         func(context.Context) ([]byte, error)
	log              func(PluginLogLevel, string, ...any)
	storageMu        sync.RWMutex
	mu               sync.Mutex
	repository       ReferencePriceRepository
	closed           bool
	metadata         ReferencePriceMetadata
	refreshSequence  uint64
	refreshDone      chan struct{}
	lastRefreshError error
	entries          map[string]*list.Element
	recency          list.List
}

func openReferencePrices(repository Repository, download func(context.Context) ([]byte, error), log func(PluginLogLevel, string, ...any)) (*referencePriceManager, error) {
	referenceRepository, err := repository.OpenReferencePrices()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), referencePriceOperationTimeout)
	defer cancel()
	metadata, err := referenceRepository.LoadReferencePriceMetadata(ctx)
	if err != nil {
		_ = referenceRepository.Close()
		return nil, err
	}
	return &referencePriceManager{
		download:   download,
		log:        log,
		repository: referenceRepository,
		metadata:   metadata,
		entries:    make(map[string]*list.Element),
	}, nil
}

func (references *referencePriceManager) close() {
	if references == nil {
		return
	}
	references.storageMu.Lock()
	defer references.storageMu.Unlock()
	references.closed = true
	_ = references.repository.Close()
}

func (references *referencePriceManager) status() (ReferencePriceMetadata, uint64) {
	if references == nil {
		return ReferencePriceMetadata{}, 0
	}
	references.mu.Lock()
	defer references.mu.Unlock()
	metadata := references.metadata
	metadata.Refreshing = references.refreshDone != nil
	return metadata, references.refreshSequence
}

func (s *Store) ReferencePriceMetadata() ReferencePriceMetadata {
	metadata, _ := s.referencePrices.Load().status()
	return metadata
}

// The upstream model takes precedence. Fall back to the requested model only
// when the upstream model has no entries, never when its prices conflict.
func referencePriceKeys(upstream, requested string) []string {
	keys := make([]string, 0, 2)
	for index, model := range []string{upstream, requested} {
		base := ModelWithoutThinkingSuffix(model)
		if index == 1 && strings.EqualFold(base, "auto") {
			continue
		}
		key := ReferenceModelKey(base)
		if key != "" && (len(keys) == 0 || keys[0] != key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func matchReferencePriceKeys(prices []ReferencePrice, keys []string) (ReferencePrice, bool) {
	for _, key := range keys {
		for _, price := range prices {
			if ReferenceModelKey(price.ModelID) == key {
				return matchReferencePriceKey(key, prices)
			}
		}
	}
	return ReferencePrice{}, false
}

func (references *referencePriceManager) lookup(ctx context.Context, upstream, model string) (referencePriceMatch, error) {
	if references == nil {
		return referencePriceMatch{}, nil
	}
	keys := referencePriceKeys(upstream, model)
	key := strings.Join(keys, "\x00")
	references.mu.Lock()
	if element := references.entries[key]; element != nil {
		references.recency.MoveToFront(element)
		entry := element.Value.(referencePriceCacheEntry)
		match := referencePriceMatch{
			price:           entry.price,
			found:           entry.found,
			metadata:        references.metadata,
			refreshSequence: references.refreshSequence,
		}
		references.mu.Unlock()
		return match, nil
	}
	references.mu.Unlock()
	references.storageMu.RLock()
	defer references.storageMu.RUnlock()
	if references.closed {
		return referencePriceMatch{}, fmt.Errorf("The reference price database changed")
	}
	references.mu.Lock()
	metadata, refreshSequence := references.metadata, references.refreshSequence
	references.mu.Unlock()
	if !metadata.Usable {
		return referencePriceMatch{metadata: metadata, refreshSequence: refreshSequence}, nil
	}
	prices, err := references.repository.ReferencePriceCandidates(ctx, keys)
	if err != nil {
		return referencePriceMatch{}, err
	}
	price, found := matchReferencePriceKeys(prices, keys)
	references.mu.Lock()
	defer references.mu.Unlock()
	if element := references.entries[key]; element != nil {
		references.recency.Remove(element)
	}
	references.entries[key] = references.recency.PushFront(referencePriceCacheEntry{key: key, price: price, found: found})
	if references.recency.Len() > referencePriceCacheCapacity {
		element := references.recency.Back()
		delete(references.entries, element.Value.(referencePriceCacheEntry).key)
		references.recency.Remove(element)
	}
	return referencePriceMatch{
		price:           price,
		found:           found,
		metadata:        references.metadata,
		refreshSequence: references.refreshSequence,
	}, nil
}

func referencePricesNeedRefresh(metadata ReferencePriceMetadata, found bool, now time.Time) bool {
	if !metadata.Usable || metadata.FetchedAt.IsZero() {
		return true
	}
	age := now.Sub(metadata.FetchedAt)
	return age > 24*time.Hour || (!found && age > time.Hour)
}

// refresh executes in the caller, or joins the current caller's operation. An
// observed refreshSequence prevents waiters from issuing a second download after it.
func (references *referencePriceManager) refresh(ctx context.Context, now func() time.Time, force bool, observed uint64, found bool) (ReferencePriceMetadata, error) {
	if references == nil {
		return ReferencePriceMetadata{}, fmt.Errorf("The reference price database is not ready")
	}
	references.mu.Lock()
	if refreshDone := references.refreshDone; refreshDone != nil {
		references.mu.Unlock()
		select {
		case <-refreshDone:
			references.mu.Lock()
			metadata, err := references.metadata, references.lastRefreshError
			references.mu.Unlock()
			return metadata, err
		case <-ctx.Done():
			return ReferencePriceMetadata{}, ctx.Err()
		}
	}
	if observed != references.refreshSequence {
		metadata, err := references.metadata, references.lastRefreshError
		references.mu.Unlock()
		return metadata, err
	}
	if !force && (!referencePricesNeedRefresh(references.metadata, found, now()) || now().Before(references.metadata.RetryAfter)) {
		metadata := references.metadata
		references.mu.Unlock()
		return metadata, nil
	}
	references.refreshDone = make(chan struct{})
	previous := references.metadata
	references.mu.Unlock()

	references.log(PluginLogInfo, "Syncing reference prices from models.dev")
	next, err := references.downloadAndCommit(ctx, now)
	if err != nil {
		next = previous
		next.SourceURL = ModelsDevPricesURL
		next.LastAttemptAt = now()
		next.ConsecutiveFailures++
		next.RetryAfter = now().Add(min(time.Minute*time.Duration(1<<min(next.ConsecutiveFailures-1, 6)), time.Hour))
		next.LastError = err.Error()
		// Persist retry metadata independently of the expired download context.
		retryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		references.storageMu.Lock()
		if !references.closed {
			if saveErr := references.repository.SaveReferencePrices(retryCtx, next, nil); saveErr != nil {
				err = &referencePriceStorageError{saveErr}
			}
		}
		references.storageMu.Unlock()
		cancel()
	}
	references.mu.Lock()
	references.metadata = next
	references.lastRefreshError = err
	references.refreshSequence++
	close(references.refreshDone)
	references.refreshDone = nil
	references.mu.Unlock()
	switch {
	case err != nil:
		references.log(PluginLogError, "Failed to sync models.dev reference prices: %v", err)
	case next.Version == previous.Version:
		references.log(PluginLogInfo, "models.dev reference prices are unchanged; refreshed fetch time for %d models", next.ModelCount)
	default:
		references.log(PluginLogInfo, "Updated models.dev reference prices for %d models", next.ModelCount)
	}
	return next, err
}

func (references *referencePriceManager) downloadAndCommit(ctx context.Context, now func() time.Time) (ReferencePriceMetadata, error) {
	raw, err := references.download(ctx)
	if err != nil {
		return ReferencePriceMetadata{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	references.storageMu.RLock()
	if references.closed {
		references.storageMu.RUnlock()
		return ReferencePriceMetadata{}, fmt.Errorf("The reference price database changed")
	}
	current, err := references.repository.LoadReferencePriceMetadata(ctx)
	references.storageMu.RUnlock()
	if err != nil {
		return ReferencePriceMetadata{}, &referencePriceStorageError{err}
	}
	unchanged := current.Usable && hash == current.ContentHash
	next := current
	var prices []ReferencePrice
	if !unchanged {
		prices, err = parseModelsDevPrices(raw)
		if err != nil {
			return ReferencePriceMetadata{}, err
		}
		if err = ctx.Err(); err != nil {
			return ReferencePriceMetadata{}, err
		}
		next.Version++
		next.ModelCount = len(prices)
		next.ContentHash = hash
		next.Usable = true
	}
	next.SourceURL = ModelsDevPricesURL
	next.FetchedAt = now()
	next.LastAttemptAt = next.FetchedAt
	next.LastError = ""
	next.ConsecutiveFailures = 0
	next.RetryAfter = time.Time{}
	references.storageMu.Lock()
	defer references.storageMu.Unlock()
	if references.closed {
		return ReferencePriceMetadata{}, fmt.Errorf("The reference price database changed")
	}
	if err = references.repository.SaveReferencePrices(ctx, next, prices); err != nil {
		return ReferencePriceMetadata{}, &referencePriceStorageError{err}
	}
	references.mu.Lock()
	references.metadata = next
	if !unchanged {
		references.entries = map[string]*list.Element{}
		references.recency.Init()
	}
	references.mu.Unlock()
	return next, nil
}

func (s *Store) EnsureReferencePrices() (ReferencePriceMetadata, error) {
	ctx, cancel := context.WithTimeout(context.Background(), referencePriceOperationTimeout)
	defer cancel()
	references := s.referencePrices.Load()
	_, refreshSequence := references.status()
	return references.refresh(ctx, s.Now, false, refreshSequence, true)
}

type ReferencePriceRefreshResult struct {
	Metadata ReferencePriceMetadata `json:"metadata"`
	Changed  bool                   `json:"changed"`
}

func (s *Store) RefreshReferencePrices() (ReferencePriceRefreshResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), referencePriceOperationTimeout)
	defer cancel()
	references := s.referencePrices.Load()
	before, refreshSequence := references.status()
	metadata, err := references.refresh(ctx, s.Now, true, refreshSequence, true)
	return ReferencePriceRefreshResult{Metadata: metadata, Changed: before.Version != metadata.Version}, err
}

func (s *Store) SearchReferencePrices(query string, limit int) ([]ReferencePrice, error) {
	references := s.referencePrices.Load()
	if references == nil {
		return []ReferencePrice{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	references.storageMu.RLock()
	defer references.storageMu.RUnlock()
	if references.closed {
		return nil, fmt.Errorf("The reference price database changed")
	}
	return references.repository.SearchReferencePrices(ctx, query, min(max(limit, 1), 50))
}

// lookupModels reads one reference-price version for display without changing the
// request cache. Batches keep SQLite parameters bounded even for large model lists.
func (references *referencePriceManager) lookupModels(ctx context.Context, models []string) (map[string]ReferencePrice, error) {
	matched := make(map[string]ReferencePrice)
	if references == nil || len(models) == 0 {
		return matched, nil
	}
	references.storageMu.RLock()
	defer references.storageMu.RUnlock()
	if references.closed {
		return nil, fmt.Errorf("The reference price database changed")
	}
	metadata, _ := references.status()
	if !metadata.Usable {
		return matched, nil
	}
	const batchSize = 128
	for start := 0; start < len(models); start += batchSize {
		batch := models[start:min(start+batchSize, len(models))]
		keys := make([]string, 0, len(batch))
		seen := make(map[string]bool)
		for _, model := range batch {
			key := ReferenceModelKey(ModelWithoutThinkingSuffix(model))
			if key != "" && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
		prices, err := references.repository.ReferencePriceCandidates(ctx, keys)
		if err != nil {
			return nil, err
		}
		candidatesByKey := make(map[string][]ReferencePrice)
		for _, price := range prices {
			key := ReferenceModelKey(price.ModelID)
			candidatesByKey[key] = append(candidatesByKey[key], price)
		}
		for _, model := range batch {
			key := ReferenceModelKey(ModelWithoutThinkingSuffix(model))
			if price, found := matchReferencePriceKey(key, candidatesByKey[key]); found {
				matched[model] = price
			}
		}
	}
	return matched, nil
}

// ReferenceModelKey removes namespaces and normalizes case in models.dev IDs.
// Request options are removed with ModelWithoutThinkingSuffix before lookup.
func ReferenceModelKey(modelID string) string {
	key := NormalizeModelID(modelID)
	if i := strings.LastIndex(key, "/"); i >= 0 {
		key = key[i+1:]
	}
	return key
}

// MatchReferencePrice prefers the canonical models.dev source. Without one,
// all matching providers must agree on supported rates. An absent rate or a
// conflict leaves the model unpriced.
func MatchReferencePrice(modelID string, candidates []ReferencePrice) (ReferencePrice, bool) {
	return matchReferencePriceKey(ReferenceModelKey(ModelWithoutThinkingSuffix(modelID)), candidates)
}

func matchReferencePriceKey(key string, candidates []ReferencePrice) (ReferencePrice, bool) {
	if key == "" {
		return ReferencePrice{}, false
	}
	canonical := false
	for _, price := range candidates {
		if price.IsCanonical && ReferenceModelKey(price.ModelID) == key {
			canonical = true
			break
		}
	}
	var matched ReferencePrice
	found := false
	for _, price := range candidates {
		if ReferenceModelKey(price.ModelID) != key || (canonical && !price.IsCanonical) {
			continue
		}
		if price.PriceRates == nil {
			return ReferencePrice{}, false
		}
		if found && !samePriceRates(*matched.PriceRates, *price.PriceRates) {
			return ReferencePrice{}, false
		}
		if !found {
			matched = price
			found = true
		}
	}
	return matched, found
}
