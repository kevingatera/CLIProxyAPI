package usage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// detailRetention bounds per-request details. Totals and daily/hourly counts are kept.
const detailRetention = 7 * 24 * time.Hour

// pruneInterval limits how often Record scans for expired details.
const pruneInterval = time.Hour

const httpStatusBadRequest = 400

// RequestDetail stores one request. JSON tags match the snapshot format. RequestID is the
// execution ID and is the deduplication key on import and restore.
type RequestDetail struct {
	RequestID string     `json:"request_id"`
	Timestamp time.Time  `json:"timestamp"`
	AuthType  string     `json:"auth_type"`
	Source    string     `json:"source"`
	Provider  string     `json:"provider,omitempty"`
	AuthIndex string     `json:"auth_index"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

// TokenStats holds token counts for one request.
type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

// StatisticsSnapshot is the serialized aggregate. APIs is keyed by KeyFingerprint or a route label.
type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]APISnapshot `json:"apis"`

	RequestsByDay  map[string]int64 `json:"requests_by_day"`
	RequestsByHour map[string]int64 `json:"requests_by_hour"`
	TokensByDay    map[string]int64 `json:"tokens_by_day"`
	TokensByHour   map[string]int64 `json:"tokens_by_hour"`
}

// APISnapshot aggregates one API key or route.
type APISnapshot struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]ModelSnapshot `json:"models"`
}

// ModelSnapshot aggregates one model within an API key or route.
type ModelSnapshot struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []RequestDetail `json:"details"`
}

// MergeResult reports how many imported details were added or skipped as duplicates.
type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

type apiStats struct {
	TotalRequests int64
	TotalTokens   int64
	Models        map[string]*modelStats
}

type modelStats struct {
	TotalRequests int64
	TotalTokens   int64
	Details       []RequestDetail
}

// RequestStatistics maintains aggregated request metrics in memory.
type RequestStatistics struct {
	mu sync.Mutex

	// collectMu fences the usage plugin against SetCollecting(false). Plugin calls hold
	// the read lock while recording; turning collection off takes the write lock, so no
	// record can land after it returns. collecting is guarded by collectMu.
	collectMu  sync.RWMutex
	collecting bool

	totalRequests int64
	successCount  int64
	failureCount  int64
	totalTokens   int64

	apis map[string]*apiStats

	requestsByDay  map[string]int64
	requestsByHour map[int]int64
	tokensByDay    map[string]int64
	tokensByHour   map[int]int64

	lastPrune time.Time

	// loadedPaths records files already merged by restoreSnapshot.
	loadedPaths map[string]struct{}
}

// NewRequestStatistics returns an empty aggregate.
func NewRequestStatistics() *RequestStatistics {
	return &RequestStatistics{
		apis:           make(map[string]*apiStats),
		requestsByDay:  make(map[string]int64),
		requestsByHour: make(map[int]int64),
		tokensByDay:    make(map[string]int64),
		tokensByHour:   make(map[int]int64),
		loadedPaths:    make(map[string]struct{}),
	}
}

// SetCollecting turns recording from the usage plugin on or off. It returns after
// in-flight plugin calls finish, so a later Snapshot includes every record admitted before
// collection stopped. Persistence enables it only when configured.
func (s *RequestStatistics) SetCollecting(enabled bool) {
	if s == nil {
		return
	}
	s.collectMu.Lock()
	s.collecting = enabled
	s.collectMu.Unlock()
}

// Collecting reports whether the usage plugin records into the aggregate.
func (s *RequestStatistics) Collecting() bool {
	if s == nil {
		return false
	}
	s.collectMu.RLock()
	defer s.collectMu.RUnlock()
	return s.collecting
}

// RecordCollected records a plugin delivery while collection is on. It holds the
// collection fence for the whole call.
func (s *RequestStatistics) RecordCollected(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	s.collectMu.RLock()
	defer s.collectMu.RUnlock()
	if !s.collecting {
		return
	}
	s.Record(ctx, record)
}

// Reset empties the aggregate and forgets which files were loaded. Callers must stop
// collection first.
func (s *RequestStatistics) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalRequests, s.successCount, s.failureCount, s.totalTokens = 0, 0, 0, 0
	s.apis = make(map[string]*apiStats)
	s.requestsByDay = make(map[string]int64)
	s.requestsByHour = make(map[int]int64)
	s.tokensByDay = make(map[string]int64)
	s.tokensByHour = make(map[int]int64)
	s.lastPrune = time.Time{}
	s.loadedPaths = make(map[string]struct{})
}

// Record aggregates one usage record. It reads only the record, never the request
// context, because plugin delivery is asynchronous. The API key is stored only as a
// fingerprint.
func (s *RequestStatistics) Record(_ context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	detail := normaliseTokenStats(TokenStats{
		InputTokens:     record.Detail.InputTokens,
		OutputTokens:    record.Detail.OutputTokens,
		ReasoningTokens: record.Detail.ReasoningTokens,
		CachedTokens:    record.Detail.CachedTokens,
		TotalTokens:     record.Detail.TotalTokens,
	})
	totalTokens := detail.TotalTokens

	requestID := strings.TrimSpace(record.RequestID)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	authType := strings.ToLower(strings.TrimSpace(record.AuthType))
	if authType == "" {
		authType = "unknown"
	}
	statsKey := apiName(record.APIKey)
	modelName := strings.TrimSpace(record.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	failed := record.Failed
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()

	s.mu.Lock()
	defer s.mu.Unlock()

	if now := time.Now(); now.Sub(s.lastPrune) >= pruneInterval {
		s.pruneLocked(now)
	}

	s.totalRequests++
	if failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens

	stats, ok := s.apis[statsKey]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[statsKey] = stats
	}
	s.updateAPIStats(stats, modelName, RequestDetail{
		RequestID: requestID,
		Timestamp: timestamp,
		AuthType:  authType,
		Source:    sourceName(authType, record.Source),
		Provider:  record.Provider,
		AuthIndex: record.AuthIndex,
		Tokens:    detail,
		Failed:    failed,
	})

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
}

// pruneLocked drops request details older than detailRetention. Totals stay.
// The caller must hold s.mu.
func (s *RequestStatistics) pruneLocked(now time.Time) {
	cutoff := now.Add(-detailRetention)
	for _, stats := range s.apis {
		for _, model := range stats.Models {
			kept := model.Details[:0]
			for _, detail := range model.Details {
				if !detail.Timestamp.Before(cutoff) {
					kept = append(kept, detail)
				}
			}
			model.Details = kept
		}
	}
	s.lastPrune = now
}

func (s *RequestStatistics) updateAPIStats(stats *apiStats, model string, detail RequestDetail) {
	stats.TotalRequests++
	stats.TotalTokens += detail.Tokens.TotalTokens
	modelStatsValue, ok := stats.Models[model]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[model] = modelStatsValue
	}
	modelStatsValue.TotalRequests++
	modelStatsValue.TotalTokens += detail.Tokens.TotalTokens
	modelStatsValue.Details = append(modelStatsValue.Details, detail)
}

// Snapshot returns a copy of the aggregate with expired details removed.
func (s *RequestStatistics) Snapshot() StatisticsSnapshot {
	result := StatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens

	result.APIs = make(map[string]APISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := APISnapshot{
			TotalRequests: stats.TotalRequests,
			TotalTokens:   stats.TotalTokens,
			Models:        make(map[string]ModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			details := make([]RequestDetail, len(modelStatsValue.Details))
			copy(details, modelStatsValue.Details)
			apiSnapshot.Models[modelName] = ModelSnapshot{
				TotalRequests: modelStatsValue.TotalRequests,
				TotalTokens:   modelStatsValue.TotalTokens,
				Details:       details,
			}
		}
		result.APIs[apiName] = apiSnapshot
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for k, v := range s.requestsByDay {
		result.RequestsByDay[k] = v
	}
	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		result.RequestsByHour[formatHour(hour)] = v
	}
	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for k, v := range s.tokensByDay {
		result.TokensByDay[k] = v
	}
	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		result.TokensByHour[formatHour(hour)] = v
	}
	return result
}

// ErrOverlappingSnapshot reports a snapshot with totals for requests older than the
// detail retention window. Those totals cannot be deduplicated, so they merge only
// into an empty aggregate.
var ErrOverlappingSnapshot = errors.New("usage snapshot has totals older than the retention window and cannot merge into a non-empty aggregate")

// MergeSnapshot imports a snapshot. Retained details already present are skipped.
// A snapshot with totals for pruned requests is rejected unless the aggregate is
// empty, so importing the same export twice cannot count it twice. A rejected
// import changes nothing.
func (s *RequestStatistics) MergeSnapshot(snapshot StatisticsSnapshot) (MergeResult, error) {
	return s.mergeSnapshot(snapshot, "")
}

// restoreSnapshot merges a snapshot loaded from path. Each path merges at most once
// per aggregate, so reloading the same file in one process does not count it twice.
func (s *RequestStatistics) restoreSnapshot(path string, snapshot StatisticsSnapshot) (MergeResult, error) {
	return s.mergeSnapshot(snapshot, filepath.Clean(path))
}

// retainedCounts tallies the details kept as records, so the remainder for pruned
// requests can be derived from the snapshot totals.
type retainedCounts struct {
	success, failure                         int64
	reqByDay, tokByDay, reqByHour, tokByHour map[string]int64
}

func newRetainedCounts() retainedCounts {
	return retainedCounts{
		reqByDay:  make(map[string]int64),
		tokByDay:  make(map[string]int64),
		reqByHour: make(map[string]int64),
		tokByHour: make(map[string]int64),
	}
}

func (c *retainedCounts) add(detail RequestDetail) {
	if detail.Failed {
		c.failure++
	} else {
		c.success++
	}
	day := detail.Timestamp.Format("2006-01-02")
	hour := formatHour(detail.Timestamp.Hour())
	c.reqByDay[day]++
	c.tokByDay[day] += detail.Tokens.TotalTokens
	c.reqByHour[hour]++
	c.tokByHour[hour] += detail.Tokens.TotalTokens
}

type plannedDetail struct {
	apiName   string
	modelName string
	detail    RequestDetail
}

type prunedTotals struct {
	apiName   string
	modelName string
	requests  int64
	tokens    int64
}

func (s *RequestStatistics) mergeSnapshot(snapshot StatisticsSnapshot, loadKey string) (MergeResult, error) {
	result := MergeResult{}
	if s == nil {
		return result, nil
	}
	cutoff := time.Now().Add(-detailRetention)

	// Plan the whole merge before touching the aggregate, so a rejected snapshot changes nothing.
	var planned []plannedDetail
	var pruned []prunedTotals
	retained := newRetainedCounts()
	for apiName, apiSnapshot := range snapshot.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			var modelReq, modelTok int64
			for _, detail := range modelSnapshot.Details {
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				if detail.Timestamp.Before(cutoff) {
					continue
				}
				retained.add(detail)
				modelReq++
				modelTok += detail.Tokens.TotalTokens
				planned = append(planned, plannedDetail{apiName: apiName, modelName: modelName, detail: detail})
			}
			pruned = append(pruned, prunedTotals{
				apiName:   apiName,
				modelName: modelName,
				requests:  nonNegative(modelSnapshot.TotalRequests - modelReq),
				tokens:    nonNegative(modelSnapshot.TotalTokens - modelTok),
			})
		}
	}
	successRem := nonNegative(snapshot.SuccessCount - retained.success)
	failureRem := nonNegative(snapshot.FailureCount - retained.failure)
	remReqDay := remainderMap(snapshot.RequestsByDay, retained.reqByDay)
	remTokDay := remainderMap(snapshot.TokensByDay, retained.tokByDay)
	remReqHour := remainderMap(snapshot.RequestsByHour, retained.reqByHour)
	remTokHour := remainderMap(snapshot.TokensByHour, retained.tokByHour)

	hasPruned := successRem > 0 || failureRem > 0 ||
		len(remReqDay) > 0 || len(remTokDay) > 0 || len(remReqHour) > 0 || len(remTokHour) > 0
	for _, p := range pruned {
		if p.requests > 0 || p.tokens > 0 {
			hasPruned = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if loadKey != "" {
		if _, loaded := s.loadedPaths[loadKey]; loaded {
			return result, nil
		}
	}
	if s.totalRequests > 0 && hasPruned {
		return MergeResult{}, ErrOverlappingSnapshot
	}

	seen := make(map[string]struct{})
	for _, stats := range s.apis {
		for _, modelStatsValue := range stats.Models {
			for _, detail := range modelStatsValue.Details {
				seen[detail.RequestID] = struct{}{}
			}
		}
	}
	for apiName := range snapshot.APIs {
		s.apiStatsFor(apiName)
	}
	for _, p := range planned {
		key := p.detail.RequestID
		if _, exists := seen[key]; exists {
			result.Skipped++
			continue
		}
		seen[key] = struct{}{}
		s.recordImported(s.apiStatsFor(p.apiName), p.modelName, p.detail)
		result.Added++
	}
	for _, p := range pruned {
		s.addPrunedTotals(s.apiStatsFor(p.apiName), p.modelName, p.requests, p.tokens)
	}
	s.successCount += successRem
	s.failureCount += failureRem
	addRemainderMap(s.requestsByDay, remReqDay)
	addRemainderMap(s.tokensByDay, remTokDay)
	addRemainderHourMap(s.requestsByHour, remReqHour)
	addRemainderHourMap(s.tokensByHour, remTokHour)

	if loadKey != "" {
		s.loadedPaths[loadKey] = struct{}{}
	}
	return result, nil
}

// apiStatsFor returns the aggregate entry for apiName, creating it if needed. The caller holds s.mu.
func (s *RequestStatistics) apiStatsFor(apiName string) *apiStats {
	stats, ok := s.apis[apiName]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[apiName] = stats
	}
	return stats
}

// addPrunedTotals adds request and token totals that have no retained detail.
func (s *RequestStatistics) addPrunedTotals(stats *apiStats, modelName string, requests, tokens int64) {
	requests, tokens = nonNegative(requests), nonNegative(tokens)
	if requests == 0 && tokens == 0 {
		return
	}
	s.totalRequests += requests
	s.totalTokens += tokens
	stats.TotalRequests += requests
	stats.TotalTokens += tokens
	modelStatsValue, ok := stats.Models[modelName]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[modelName] = modelStatsValue
	}
	modelStatsValue.TotalRequests += requests
	modelStatsValue.TotalTokens += tokens
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// remainderMap returns the positive part of total minus retained, per key.
func remainderMap(total, retained map[string]int64) map[string]int64 {
	out := make(map[string]int64)
	for key, value := range total {
		if remainder := nonNegative(value - retained[key]); remainder > 0 {
			out[key] = remainder
		}
	}
	return out
}

func addRemainderMap(dst map[string]int64, remainder map[string]int64) {
	for key, value := range remainder {
		dst[key] += value
	}
}

func addRemainderHourMap(dst map[int]int64, remainder map[string]int64) {
	for key, value := range remainder {
		hour, err := strconv.Atoi(key)
		if err != nil || hour < 0 || hour > 23 {
			continue
		}
		dst[hour] += value
	}
}

func (s *RequestStatistics) recordImported(stats *apiStats, modelName string, detail RequestDetail) {
	totalTokens := detail.Tokens.TotalTokens
	if totalTokens < 0 {
		totalTokens = 0
	}
	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens
	s.updateAPIStats(stats, modelName, detail)

	dayKey := detail.Timestamp.Format("2006-01-02")
	hourKey := detail.Timestamp.Hour()
	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
}

func normaliseTokenStats(tokens TokenStats) TokenStats {
	tokens.InputTokens = nonNegative(tokens.InputTokens)
	tokens.OutputTokens = nonNegative(tokens.OutputTokens)
	tokens.ReasoningTokens = nonNegative(tokens.ReasoningTokens)
	tokens.CachedTokens = nonNegative(tokens.CachedTokens)
	tokens.TotalTokens = nonNegative(tokens.TotalTokens)
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	return fmt.Sprintf("%02d", hour%24)
}

var defaultRequestStatistics = NewRequestStatistics()

// GetRequestStatistics returns the process-wide aggregate.
func GetRequestStatistics() *RequestStatistics { return defaultRequestStatistics }
