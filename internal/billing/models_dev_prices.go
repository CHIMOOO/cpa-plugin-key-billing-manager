package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	ModelsDevPricesURL             = "https://models.dev/catalog.json"
	maxReferencePriceResponseBytes = 128 << 20
	referencePriceHTTPTimeout      = 30 * time.Second
)

type modelsDevResponse struct {
	Models    map[string]json.RawMessage   `json:"models"`
	Providers map[string]modelsDevProvider `json:"providers"`
}

type modelsDevProvider struct {
	ID     string                     `json:"id"`
	Models map[string]json.RawMessage `json:"models"`
}

type modelsDevModel struct {
	ID   string         `json:"id"`
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input       *float64            `json:"input"`
	Output      *float64            `json:"output"`
	Reasoning   *float64            `json:"reasoning"`
	CacheRead   *float64            `json:"cache_read"`
	CacheWrite  *float64            `json:"cache_write"`
	InputAudio  *float64            `json:"input_audio"`
	OutputAudio *float64            `json:"output_audio"`
	Tiers       []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input       *float64 `json:"input"`
	Output      *float64 `json:"output"`
	Reasoning   *float64 `json:"reasoning"`
	CacheRead   *float64 `json:"cache_read"`
	CacheWrite  *float64 `json:"cache_write"`
	InputAudio  *float64 `json:"input_audio"`
	OutputAudio *float64 `json:"output_audio"`
	Tier        struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

func parseModelsDevPrices(raw []byte) ([]ReferencePrice, error) {
	var source modelsDevResponse
	if errUnmarshal := json.Unmarshal(raw, &source); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	canonicalModels := make(map[string]struct{}, len(source.Models))
	for name := range source.Models {
		canonicalModels[NormalizeModelID(name)] = struct{}{}
	}

	prices := []ReferencePrice{}
	pricedModels := 0
	seen := map[string]bool{}
	for providerKey, provider := range source.Providers {
		providerID := modelsDevID(provider.ID, providerKey)
		for modelKey, rawModel := range provider.Models {
			var model modelsDevModel
			if err := json.Unmarshal(rawModel, &model); err != nil {
				return nil, fmt.Errorf("Read models.dev model %s/%s: %w", providerID, modelKey, err)
			}
			modelID := modelsDevID(model.ID, modelKey)
			identity := providerID + "\x00" + modelID
			if seen[identity] {
				return nil, fmt.Errorf("Duplicate models.dev model identity: %s/%s", providerID, modelID)
			}
			seen[identity] = true
			// The top-level model namespace identifies the originating provider.
			_, canonical := canonicalModels[NormalizeModelID(providerID+"/"+modelID)]
			price := ReferencePrice{ProviderID: providerID, ModelID: modelID, IsCanonical: canonical}
			if err := price.Validate(); err != nil {
				return nil, err
			}
			// Keep models without supported rates, especially canonical entries:
			// a missing canonical price must not select a reseller's price instead.
			if rates := modelsDevPriceRates(model.Cost); rates != nil && rates.validate(modelID) == nil {
				price.PriceRates = rates
				pricedModels++
			}
			prices = append(prices, price)
		}
	}
	if pricedModels == 0 {
		return nil, fmt.Errorf("models.dev returned no usable reference prices")
	}
	sort.Slice(prices, func(i, j int) bool {
		if prices[i].ProviderID != prices[j].ProviderID {
			return prices[i].ProviderID < prices[j].ProviderID
		}
		return prices[i].ModelID < prices[j].ModelID
	})
	return prices, nil
}

func modelsDevID(id, defaultID string) string {
	if strings.TrimSpace(id) != "" {
		return id
	}
	return defaultID
}

func modelsDevPriceRates(value *modelsDevCost) *PriceRates {
	// Billing supports one context threshold and no separate audio/reasoning rates.
	if value == nil || value.Input == nil || value.Output == nil || len(value.Tiers) > 1 ||
		!optionalPriceMatches(value.Reasoning, value.Output) ||
		!optionalPriceMatches(value.InputAudio, value.Input) ||
		!optionalPriceMatches(value.OutputAudio, value.Output) {
		return nil
	}
	rates := &PriceRates{
		InputPer1M:      *value.Input,
		OutputPer1M:     *value.Output,
		CacheReadPer1M:  value.CacheRead,
		CacheWritePer1M: value.CacheWrite,
	}
	if len(value.Tiers) == 1 {
		tier := value.Tiers[0]
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 || tier.Input == nil || tier.Output == nil ||
			!optionalPriceMatches(tier.Reasoning, tier.Output) ||
			!optionalPriceMatches(tier.InputAudio, tier.Input) ||
			!optionalPriceMatches(tier.OutputAudio, tier.Output) {
			return nil
		}
		rates.LongContext = &LongContextPrice{
			ThresholdInputTokens: tier.Tier.Size,
			InputPer1M:           *tier.Input,
			OutputPer1M:          *tier.Output,
			CacheReadPer1M:       tier.CacheRead,
			CacheWritePer1M:      tier.CacheWrite,
		}
	}
	return rates
}

func optionalPriceMatches(special, standard *float64) bool {
	return special == nil || (standard != nil && *special == *standard)
}

func downloadModelsDevPrices(ctx context.Context) ([]byte, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	defer transport.CloseIdleConnections()
	return fetchModelsDevPrices(ctx, &http.Client{Timeout: referencePriceHTTPTimeout, Transport: transport})
}

func fetchModelsDevPrices(ctx context.Context, client *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevPricesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("Create reference price request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Do not include URL credentials or query parameters from transport errors.
		var requestError *url.Error
		for errors.As(err, &requestError) {
			err = requestError.Err
		}
		return nil, fmt.Errorf("Download reference prices: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Download reference prices: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReferencePriceResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Read reference prices: %w", err)
	}
	if len(raw) > maxReferencePriceResponseBytes {
		return nil, fmt.Errorf("Reference price response exceeds the size limit")
	}
	return raw, nil
}
