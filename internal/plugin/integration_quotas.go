package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (a *App) integrationObject(callbackID, endpoint, token string, headers http.Header) (map[string]any, error) {
	if token == "" {
		return nil, fmt.Errorf("An integration API credential is required")
	}
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+token)
	response, err := a.integrationHTTP(callbackID, http.MethodGet, endpoint, headers, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Subscription quota request returned HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if json.Unmarshal(response.Body, &payload) != nil {
		return nil, fmt.Errorf("Subscription quota response has an invalid format")
	}
	if success, exists := payload["success"].(bool); exists && !success {
		return nil, fmt.Errorf("Subscription quota request was rejected")
	}
	return payload, nil
}

// These are the endpoints used by the public Command Code CLI usage command.
// Organization selection comes exclusively from whoami for the same token.
func (a *App) fetchCommandCodeQuota(callbackID string, account integrationAccount, result *authQuotaResponse) error {
	const base = "https://api.commandcode.ai"
	whoami, err := a.integrationObject(callbackID, base+"/alpha/whoami?limits=1", account.APIKey, nil)
	if err != nil {
		return err
	}
	orgID := firstString(objectMap(whoami, "org"), "id")
	query := ""
	if orgID != "" {
		query = "?orgId=" + url.QueryEscape(orgID)
	}
	credits, err := a.integrationObject(callbackID, base+"/alpha/billing/credits"+query, account.APIKey, nil)
	if err != nil {
		return err
	}
	parseCommandCodeQuota(credits, result)
	subscription, subscriptionErr := a.integrationObject(callbackID, base+"/alpha/billing/subscriptions"+query, account.APIKey, nil)
	if subscriptionErr == nil {
		if data := objectMap(subscription, "data"); data != nil {
			if plan := firstString(data, "planId"); plan != "" {
				result.Plan = plan
			}
			result.SubscriptionStatus = firstString(data, "status")
			result.SubscriptionEndsAt = quotaResetAt(firstString(data, "currentPeriodEnd"))
		}
	}
	if len(result.Quota) == 0 {
		return fmt.Errorf("CommandCode returned no recognizable quota or credit data")
	}
	return nil
}

func parseCommandCodeQuota(payload map[string]any, result *authQuotaResponse) {
	credits := objectMap(payload, "credits")
	result.Plan = firstString(credits, "planId")
	limits := objectMap(payload, "windowLimits")
	for _, window := range []struct {
		Key, Label string
		Seconds    int64
	}{{"fiveHour", "5-hour limit", 18000}, {"weekly", "Weekly limit", 604800}} {
		values := objectMap(limits, window.Key)
		cap, hasCap := floatValue(values, "cap")
		used, hasUsed := floatValue(values, "used")
		if !hasCap || !hasUsed || cap <= 0 {
			continue
		}
		row := quotaRow{Label: window.Label, Scope: "account", WindowSeconds: window.Seconds, Currency: "USD", Used: floatPointer(used), Limit: floatPointer(cap), RemainingPercent: remainingPercent((1 - used/cap) * 100)}
		if reset, ok := intValue(values, "resetAt"); ok && reset > 0 {
			row.ResetAt = time.UnixMilli(reset).UTC().Format(time.RFC3339)
		}
		result.Quota = append(result.Quota, row)
	}
	for _, bucket := range []struct{ Key, Label string }{{"monthlyCredits", "Monthly credits"}, {"purchasedCredits", "Purchased credits"}, {"freeCredits", "Free credits"}} {
		remaining, ok := floatValue(credits, bucket.Key)
		if !ok {
			continue
		}
		row := quotaRow{Label: bucket.Label, Scope: "credits:" + bucket.Key, Currency: "USD", Remaining: floatPointer(remaining)}
		if bucket.Key == "monthlyCredits" {
			if granted, ok := floatValue(credits, "monthlyCreditsGranted"); ok && granted > 0 {
				row.Limit, row.Used, row.RemainingPercent = floatPointer(granted), floatPointer(math.Max(0, granted-remaining)), remainingPercent(remaining/granted*100)
			}
		}
		result.Quota = append(result.Quota, row)
	}
}

func (a *App) fetchClineSubscriptionQuota(callbackID string, account integrationAccount, result *authQuotaResponse) error {
	payload, err := a.integrationObject(callbackID, "https://api.cline.bot/api/v1/users/me/plan/usage-limits", account.APIKey, integrationHeaders(account.Kind))
	if err != nil {
		return err
	}
	data := objectMap(payload, "data")
	if data == nil {
		return fmt.Errorf("Cline returned no subscription usage data")
	}
	result.Plan = firstString(data, "planName", "plan")
	for _, raw := range objectSlice(data, "limits") {
		limit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		used, ok := floatValue(limit, "percentUsed")
		if !ok {
			continue
		}
		kind := firstString(limit, "type")
		if kind == "" {
			kind = "Subscription limit"
		}
		row := quotaRow{Label: cleanText(kind), Scope: "account", RemainingPercent: remainingPercent(100 - used), ResetAt: quotaResetAt(firstString(limit, "resetsAt"))}
		switch strings.ToLower(kind) {
		case "five_hour":
			row.Label = "5-hour limit"
			row.WindowSeconds = 18000
		case "weekly":
			row.Label = "Weekly limit"
			row.WindowSeconds = 604800
		case "monthly":
			row.Label = "Monthly limit"
		}
		result.Quota = append(result.Quota, row)
	}
	if len(result.Quota) == 0 {
		return fmt.Errorf("Cline returned no active subscription usage limits")
	}
	return nil
}
