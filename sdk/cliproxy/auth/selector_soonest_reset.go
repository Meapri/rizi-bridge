package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// soonestResetMaxHorizon bounds how far ahead an observed reset may lie before it is
	// treated as unknown. The longest observed quota window is weekly; the slack matches
	// the Devin quota reset horizon.
	soonestResetMaxHorizon = 8 * 24 * time.Hour
	// soonestResetGranularity is the resolution at which resets are compared. A reset
	// derived from reset-after-seconds moves with observation latency, so resets inside
	// the same minute count as simultaneous and are ordered by usage instead.
	soonestResetGranularity = time.Minute
	// maxQuotaResetEpochSeconds rejects values that cannot be epoch seconds, such as
	// epoch milliseconds, before they are converted to a time.
	maxQuotaResetEpochSeconds = 1e11
)

// SoonestResetSelector prefers the credential whose observed quota window resets
// soonest, so quota that would otherwise expire unused at the reset is spent first.
//
// Candidates are narrowed exactly like the other built-in selectors: the highest
// available priority tier, with Codex websocket-capable credentials preferred for
// downstream websocket requests. Cooldown and quota-exceeded exclusion stays with the
// shared availability filter. Credentials with a known reset are ordered by reset
// minute, then by higher used percent (draining the credential with less headroom
// first), then by ID. Credentials without a usable reset signal are used only when no
// credential with a known reset is available, and rotate round-robin among themselves.
type SoonestResetSelector struct {
	mu         sync.Mutex
	lastPicked map[string]string
	maxKeys    int
}

// quotaResetWindow names the lower-case signal keys that describe one quota window.
type quotaResetWindow struct {
	// resetAt holds epoch seconds or an RFC3339 timestamp.
	resetAt string
	// resetAfter holds seconds relative to QuotaState.ObservedAt.
	resetAfter string
	// usedPercent holds the consumed share in percent (0-100).
	usedPercent string
	// remainingPercent holds the remaining share in percent, optionally suffixed by "%".
	remainingPercent string
	// utilization holds the consumed share as a fraction (0-1).
	utilization string
	// status holds a window status where "rejected" marks an exhausted window.
	status string
}

// quotaResetWindows lists the credential-wide windows observed in Quota.Signals:
// Codex primary/secondary, Claude unified 5h/7d, and Devin daily/weekly. Codex
// additional per-model limits are deliberately ignored because they do not gate the
// credential as a whole.
var quotaResetWindows = []quotaResetWindow{
	{resetAt: "x-codex-primary-reset-at", resetAfter: "x-codex-primary-reset-after-seconds", usedPercent: "x-codex-primary-used-percent"},
	{resetAt: "x-codex-secondary-reset-at", resetAfter: "x-codex-secondary-reset-after-seconds", usedPercent: "x-codex-secondary-used-percent"},
	{resetAt: "anthropic-ratelimit-unified-5h-reset", utilization: "anthropic-ratelimit-unified-5h-utilization", status: "anthropic-ratelimit-unified-5h-status"},
	{resetAt: "anthropic-ratelimit-unified-7d-reset", utilization: "anthropic-ratelimit-unified-7d-utilization", status: "anthropic-ratelimit-unified-7d-status"},
	{resetAt: "daily_quota_reset_at", remainingPercent: "daily_quota_remaining_percent"},
	{resetAt: "weekly_quota_reset_at", remainingPercent: "weekly_quota_remaining_percent"},
}

// quotaSignalIndex is a case-insensitive view of Quota.Signals. Header-derived signals
// use canonical MIME casing while Devin signals use lower-case snake keys.
type quotaSignalIndex map[string]string

func newQuotaSignalIndex(signals map[string]string) quotaSignalIndex {
	index := make(quotaSignalIndex, len(signals))
	for key, value := range signals {
		index[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return index
}

func (index quotaSignalIndex) value(key string) string {
	if key == "" {
		return ""
	}
	return index[key]
}

// Pick selects the available auth whose quota resets soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	known, unknown := soonestResetOrder(available, now)
	if len(known) > 0 {
		return known[0], nil
	}
	if len(unknown) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]string)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.lastPicked[key]; !ok && len(s.lastPicked) >= limit {
		s.lastPicked = make(map[string]string)
	}
	picked := unknown[successorIndex(unknown, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// soonestResetOrder splits auths into credentials with a known upcoming reset, ordered
// for soonest-reset selection, and credentials without one, ordered by ID.
func soonestResetOrder(auths []*Auth, now time.Time) (known, unknown []*Auth) {
	type rankedAuth struct {
		auth        *Auth
		resetAt     time.Time
		usedPercent float64
	}
	ranked := make([]rankedAuth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		resetAt, usedPercent, ok := nextQuotaReset(auth, now)
		if !ok {
			unknown = append(unknown, auth)
			continue
		}
		ranked = append(ranked, rankedAuth{auth: auth, resetAt: resetAt.Truncate(soonestResetGranularity), usedPercent: usedPercent})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if !ranked[i].resetAt.Equal(ranked[j].resetAt) {
			return ranked[i].resetAt.Before(ranked[j].resetAt)
		}
		if ranked[i].usedPercent != ranked[j].usedPercent {
			return ranked[i].usedPercent > ranked[j].usedPercent
		}
		return ranked[i].auth.ID < ranked[j].auth.ID
	})
	known = make([]*Auth, 0, len(ranked))
	for _, entry := range ranked {
		known = append(known, entry.auth)
	}
	sort.SliceStable(unknown, func(i, j int) bool { return unknown[i].ID < unknown[j].ID })
	return known, unknown
}

// nextQuotaReset reports the earliest upcoming reset among the auth's quota windows that
// still have headroom, together with that window's used percent (-1 when not reported).
// Resets that already passed, lie beyond soonestResetMaxHorizon, or fail to parse are
// treated as unknown.
func nextQuotaReset(auth *Auth, now time.Time) (time.Time, float64, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, 0, false
	}
	signals := newQuotaSignalIndex(auth.Quota.Signals)
	var bestReset time.Time
	bestUsed := -1.0
	found := false
	for _, window := range quotaResetWindows {
		resetAt, ok := window.nextReset(signals, auth.Quota.ObservedAt, now)
		if !ok {
			continue
		}
		usedPercent, exhausted := window.usage(signals)
		if exhausted {
			continue
		}
		if !found || resetAt.Before(bestReset) || (resetAt.Equal(bestReset) && usedPercent > bestUsed) {
			bestReset, bestUsed, found = resetAt, usedPercent, true
		}
	}
	return bestReset, bestUsed, found
}

// nextReset resolves the window's reset time, preferring the absolute reset-at signal
// over reset-after-seconds, and accepts it only when it lies in the future within the
// horizon.
func (w quotaResetWindow) nextReset(signals quotaSignalIndex, observedAt, now time.Time) (time.Time, bool) {
	resetAt, ok := parseQuotaResetAt(signals.value(w.resetAt))
	if !ok && !observedAt.IsZero() {
		resetAt, ok = parseQuotaResetAfter(signals.value(w.resetAfter), observedAt)
	}
	if !ok || !resetAt.After(now) || resetAt.Sub(now) > soonestResetMaxHorizon {
		return time.Time{}, false
	}
	return resetAt, true
}

// usage reports the window's used percent (-1 when not reported) and whether the window
// has no headroom left. A window without usage signals counts as having headroom.
func (w quotaResetWindow) usage(signals quotaSignalIndex) (float64, bool) {
	if strings.EqualFold(signals.value(w.status), "rejected") {
		return 100, true
	}
	if used, ok := parseQuotaNumber(strings.TrimSuffix(signals.value(w.usedPercent), "%")); ok && used >= 0 {
		return math.Min(used, 100), used >= 100
	}
	if remaining, ok := parseQuotaNumber(strings.TrimSuffix(signals.value(w.remainingPercent), "%")); ok {
		return math.Min(math.Max(100-remaining, 0), 100), remaining <= 0
	}
	if utilization, ok := parseQuotaNumber(signals.value(w.utilization)); ok && utilization >= 0 {
		return math.Min(utilization*100, 100), utilization >= 1
	}
	return -1, false
}

// parseQuotaResetAt parses an absolute reset given as epoch seconds or RFC3339.
func parseQuotaResetAt(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, ok := parseQuotaNumber(raw); ok {
		if seconds <= 0 || seconds > maxQuotaResetEpochSeconds {
			return time.Time{}, false
		}
		whole, fraction := math.Modf(seconds)
		return time.Unix(int64(whole), int64(fraction*float64(time.Second))), true
	}
	parsed, errParse := time.Parse(time.RFC3339, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// parseQuotaResetAfter parses a reset given in seconds after the observation time.
func parseQuotaResetAfter(raw string, observedAt time.Time) (time.Time, bool) {
	seconds, ok := parseQuotaNumber(raw)
	if !ok || seconds < 0 || seconds > soonestResetMaxHorizon.Seconds() {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(seconds * float64(time.Second))), true
}

// parseQuotaNumber parses a finite decimal number.
func parseQuotaNumber(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}
