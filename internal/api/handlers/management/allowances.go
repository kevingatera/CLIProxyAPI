package management

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kevingatera/model-capacity/capacity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// CollectAllowances grants only read access to normalized, opaque account balances.
func (h *Handler) CollectAllowances(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	h.mu.Lock()
	key := h.cfg.Routing.Allowances.CollectorKey
	h.mu.Unlock()
	if len(key) < 32 {
		c.Status(http.StatusNotFound)
		return
	}
	header := c.GetHeader("Authorization")
	want := sha256.Sum256([]byte(key))
	got := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		c.Status(http.StatusUnauthorized)
		return
	}
	if h.authManager == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	h.allowanceMu.Lock()
	defer h.allowanceMu.Unlock()
	now := time.Now().UTC()
	if h.allowanceSnapshot != nil && now.Sub(h.allowanceSnapshot.GeneratedAt) < 5*time.Minute {
		c.JSON(http.StatusOK, h.allowanceSnapshot)
		return
	}
	snapshot := capacity.Snapshot{Version: capacity.Version, GeneratedAt: now, ExpiresAt: now.Add(10 * time.Minute), Accounts: []capacity.Account{}}
	groups := map[string]int{}
	for _, a := range h.authManager.List() {
		if a == nil || a.Disabled {
			continue
		}
		a.EnsureIndex()
		provider := nativeQuotaProvider(a)
		groupKey := a.ID
		if provider != "" && a.Attributes["api_key"] != "" {
			groupKey = provider + "\x00" + a.Attributes["api_key"]
		}
		if index, exists := groups[groupKey]; exists {
			snapshot.Accounts[index].CredentialIndexes = append(snapshot.Accounts[index].CredentialIndexes, a.Index)
			continue
		}
		account := capacity.Account{ID: a.Index, CredentialIndexes: []string{a.Index}, Provider: a.Provider, Status: "unavailable", ObservedAt: now, ExpiresAt: snapshot.ExpiresAt, Source: "cliproxy-native", AdapterVersion: "1"}
		var report pluginapi.QuotaFetchResponse
		var err error
		if provider != "" {
			report, err = h.fetchNativeQuota(c.Request.Context(), a)
			account.Provider = provider
		} else {
			h.mu.Lock()
			host := h.pluginHost
			h.mu.Unlock()
			if host == nil {
				continue
			}
			var handled bool
			report, handled, err = host.FetchQuota(c.Request.Context(), pluginapi.QuotaFetchRequest{AuthIndex: a.Index, AuthID: a.ID, Provider: a.Provider, Metadata: a.Metadata, Attributes: a.Attributes})
			if !handled {
				continue
			}
			account.Source = "cliproxy-plugin"
		}
		if err == nil {
			data, marshalErr := json.Marshal(report)
			if marshalErr == nil && json.Unmarshal(data, &account.Report) == nil {
				for gi := range account.Report.Groups {
					for bi := range account.Report.Groups[gi].Buckets {
						bucket := &account.Report.Groups[gi].Buckets[bi]
						if bucket.Scope == "" {
							bucket.Scope = "account"
						}
					}
				}
				account.Status = "ok"
			}
		}
		// Reject malformed adapter data without discarding other providers' reports.
		probe := capacity.Snapshot{Version: capacity.Version, GeneratedAt: now, ExpiresAt: snapshot.ExpiresAt, Accounts: []capacity.Account{account}}
		if probe.Validate(now) != nil {
			account.Status = "unavailable"
			account.Report = capacity.Report{}
		}
		groups[groupKey] = len(snapshot.Accounts)
		snapshot.Accounts = append(snapshot.Accounts, account)
	}
	h.allowanceSnapshot = &snapshot
	c.JSON(http.StatusOK, snapshot)
}

func (h *Handler) refreshAllowanceService(ctx context.Context) bool {
	h.mu.Lock()
	cfg := h.cfg.Routing.Allowances
	h.mu.Unlock()
	if cfg.ServiceURL == "" || len(cfg.ServiceKey) < 32 || h.authManager == nil {
		return false
	}
	snapshot, err := capacity.Fetch(ctx, &http.Client{}, cfg.ServiceURL, cfg.ServiceKey)
	if err != nil {
		return false
	}
	auths := h.authManager.List()
	now := time.Now()
	for _, account := range snapshot.Accounts {
		if account.Status != "ok" || !account.ExpiresAt.After(now) {
			continue
		}
		data, marshalErr := json.Marshal(account.Report)
		if marshalErr != nil {
			continue
		}
		var report pluginapi.QuotaFetchResponse
		if json.Unmarshal(data, &report) != nil {
			continue
		}
		for _, a := range auths {
			a.EnsureIndex()
			for _, index := range account.CredentialIndexes {
				if a.Index == index {
					h.authManager.ObserveUnifiedQuota(a.ID, report, account.ObservedAt)
					break
				}
			}
		}
	}
	return len(snapshot.Accounts) > 0
}

func sameNativeAccount(a, b *coreauth.Auth) bool {
	if a == nil || b == nil {
		return false
	}
	if a.ID == b.ID {
		return true
	}
	return a.Attributes["api_key"] != "" && a.Attributes["api_key"] == b.Attributes["api_key"] && nativeQuotaProvider(a) != "" && nativeQuotaProvider(a) == nativeQuotaProvider(b)
}
