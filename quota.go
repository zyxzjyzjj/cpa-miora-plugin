package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// quotaDescribe declares the quota surface for this provider.
func quotaDescribe() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerID},
		DisplayName:        "Miora 额度",
		SupportsReset:      false,
	}
}

// quotaFetch returns the normalised balance view CPA renders next to an account.
// Miora tracks credits (not tokens), so the numbers are credits with a plan label.
func quotaFetch(request []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode quota fetch request: %w", errUnmarshal)
		}
	}
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return nil, errCred
	}
	if !cred.valid() {
		return failEnvelope("missing_credential", "auth record has no Miora access token", http.StatusUnauthorized)
	}
	cfg := config()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := newAPIClient(cfg, cred)

	credits, errCredits := client.fetchCredits(ctx)
	if errCredits != nil {
		if authErr := asAuthError(errCredits); authErr != nil {
			return failEnvelope("credential_invalid", "Miora 登录态失效，请重新导入", http.StatusUnauthorized)
		}
		return failEnvelope("quota_unavailable", errCredits.Error(), http.StatusBadGateway)
	}
	plan := firstNonEmpty(credits.TierName, cred.Plan)
	if plan == "" {
		if profile, errProfile := client.fetchProfile(ctx); errProfile == nil {
			plan = profile.Plan
		}
	}

	total := credits.TotalAmount.value()
	used := credits.UsedAmount.value()
	remaining := credits.remaining()
	fraction := 0.0
	if total > 0 {
		fraction = remaining / total
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}
	}

	subscription := &pluginapi.QuotaSubscription{Plan: firstNonEmpty(plan, "free")}
	return okEnvelope(pluginapi.QuotaFetchResponse{
		Subscription: subscription,
		Summary: []pluginapi.QuotaMetric{
			{Key: "credits_total", Label: "总额度", Value: total, Unit: "credits", Format: "number"},
			{Key: "credits_used", Label: "已用", Value: used, Unit: "credits", Format: "number"},
			{Key: "credits_remaining", Label: "剩余", Value: remaining, Unit: "credits", Format: "number"},
		},
		Groups: []pluginapi.QuotaGroup{{
			DisplayName: "Miora",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "account",
				RemainingFraction: fraction,
				ResetTime:         credits.ExpiresAt,
				Description: fmt.Sprintf("剩余 %.2f / %.2f credits%s",
					remaining, total, expiryNote(credits.ExpiresAt)),
			}},
		}},
	})
}

func quotaReset() pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{Success: false, Message: "Miora 额度由上游平台管理，插件不支持重置"}
}

// usageHandle records host-side usage. Media generations carry no token counts,
// so this is a per-account request counter surfaced in the panel.
func usageHandle(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
			return okEnvelope(map[string]any{})
		}
	}
	status := "completed"
	if record.Failed {
		status = "failed"
	}
	recordEvent(eventRecord{
		Time:      firstNonZeroTime(record.RequestedAt),
		Account:   firstNonEmpty(record.AuthID, "unknown"),
		Model:     record.Model,
		Media:     record.ExecutorType,
		Status:    status,
		ElapsedMS: record.Latency.Milliseconds(),
		Error:     record.Failure.Body,
	})
	return okEnvelope(map[string]any{})
}

func firstNonZeroTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now()
	}
	return value
}

// expiryNote renders the credit expiry window for the quota description.
func expiryNote(expiresAt string) string {
	if expiresAt == "" {
		return ""
	}
	return "（" + expiresAt + " 到期）"
}
