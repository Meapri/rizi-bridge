package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func soonestResetEpoch(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

func codexResetAuth(id string, resetAt time.Time, usedPercent string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Quota: QuotaState{
			ObservedAt: time.Now(),
			Signals: map[string]string{
				"X-Codex-Primary-Reset-At":     soonestResetEpoch(resetAt),
				"X-Codex-Primary-Used-Percent": usedPercent,
			},
		},
	}
}

func TestNextQuotaReset_ParsesResetSignals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-10 * time.Minute)
	tests := []struct {
		name       string
		signals    map[string]string
		observedAt time.Time
		wantOK     bool
		wantReset  time.Time
		wantUsed   float64
	}{
		{
			name:      "codex epoch reset-at",
			signals:   map[string]string{"X-Codex-Primary-Reset-At": soonestResetEpoch(now.Add(2 * time.Hour)), "X-Codex-Primary-Used-Percent": "40"},
			wantOK:    true,
			wantReset: now.Add(2 * time.Hour),
			wantUsed:  40,
		},
		{
			name:      "rfc3339 reset-at",
			signals:   map[string]string{"X-Codex-Primary-Reset-At": now.Add(2 * time.Hour).Format(time.RFC3339)},
			wantOK:    true,
			wantReset: now.Add(2 * time.Hour),
			wantUsed:  -1,
		},
		{
			name:       "reset-after-seconds is relative to the observation",
			signals:    map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "3600", "X-Codex-Secondary-Used-Percent": "12.5"},
			observedAt: observedAt,
			wantOK:     true,
			wantReset:  observedAt.Add(time.Hour),
			wantUsed:   12.5,
		},
		{
			name:       "reset-at wins over reset-after-seconds",
			signals:    map[string]string{"X-Codex-Primary-Reset-At": soonestResetEpoch(now.Add(3 * time.Hour)), "X-Codex-Primary-Reset-After-Seconds": "60"},
			observedAt: observedAt,
			wantOK:     true,
			wantReset:  now.Add(3 * time.Hour),
			wantUsed:   -1,
		},
		{
			name:       "unparseable reset-at falls back to reset-after-seconds",
			signals:    map[string]string{"X-Codex-Primary-Reset-At": "soon", "X-Codex-Primary-Reset-After-Seconds": "1800"},
			observedAt: observedAt,
			wantOK:     true,
			wantReset:  observedAt.Add(30 * time.Minute),
			wantUsed:   -1,
		},
		{
			name:    "reset-after-seconds without observation time",
			signals: map[string]string{"X-Codex-Primary-Reset-After-Seconds": "1800"},
		},
		{
			name:    "expired reset-at",
			signals: map[string]string{"X-Codex-Primary-Reset-At": soonestResetEpoch(now.Add(-time.Minute))},
		},
		{
			name:       "expired reset-after-seconds",
			signals:    map[string]string{"X-Codex-Primary-Reset-After-Seconds": "60"},
			observedAt: now.Add(-2 * time.Hour),
		},
		{
			name:    "reset beyond the horizon",
			signals: map[string]string{"X-Codex-Secondary-Reset-At": soonestResetEpoch(now.Add(9 * 24 * time.Hour))},
		},
		{
			name:    "epoch milliseconds",
			signals: map[string]string{"X-Codex-Primary-Reset-At": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)},
		},
		{
			name:    "unparseable reset-at",
			signals: map[string]string{"X-Codex-Primary-Reset-At": "tomorrow"},
		},
		{
			name: "missing signals",
		},
		{
			name:    "unrelated signals",
			signals: map[string]string{"X-Codex-Plan-Type": "pro", "Retry-After": "60"},
		},
		{
			name:      "keys match case-insensitively",
			signals:   map[string]string{"x-codex-primary-reset-at": soonestResetEpoch(now.Add(time.Hour)), "x-codex-primary-used-percent": "7"},
			wantOK:    true,
			wantReset: now.Add(time.Hour),
			wantUsed:  7,
		},
		{
			name: "earliest of several windows with headroom",
			signals: map[string]string{
				"X-Codex-Primary-Reset-At":       soonestResetEpoch(now.Add(4 * time.Hour)),
				"X-Codex-Primary-Used-Percent":   "10",
				"X-Codex-Secondary-Reset-At":     soonestResetEpoch(now.Add(2 * time.Hour)),
				"X-Codex-Secondary-Used-Percent": "50",
			},
			wantOK:    true,
			wantReset: now.Add(2 * time.Hour),
			wantUsed:  50,
		},
		{
			name: "exhausted window is skipped",
			signals: map[string]string{
				"X-Codex-Primary-Reset-At":       soonestResetEpoch(now.Add(time.Hour)),
				"X-Codex-Primary-Used-Percent":   "100",
				"X-Codex-Secondary-Reset-At":     soonestResetEpoch(now.Add(48 * time.Hour)),
				"X-Codex-Secondary-Used-Percent": "30",
			},
			wantOK:    true,
			wantReset: now.Add(48 * time.Hour),
			wantUsed:  30,
		},
		{
			name: "all windows exhausted",
			signals: map[string]string{
				"X-Codex-Primary-Reset-At":       soonestResetEpoch(now.Add(time.Hour)),
				"X-Codex-Primary-Used-Percent":   "100",
				"X-Codex-Secondary-Reset-At":     soonestResetEpoch(now.Add(48 * time.Hour)),
				"X-Codex-Secondary-Used-Percent": "100",
			},
		},
		{
			name: "devin daily and weekly windows",
			signals: map[string]string{
				"daily_quota_reset_at":           now.Add(6 * time.Hour).Format(time.RFC3339),
				"daily_quota_remaining_percent":  "0%",
				"weekly_quota_reset_at":          now.Add(72 * time.Hour).Format(time.RFC3339),
				"weekly_quota_remaining_percent": "35%",
			},
			wantOK:    true,
			wantReset: now.Add(72 * time.Hour),
			wantUsed:  65,
		},
		{
			name: "claude unified windows",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset":       soonestResetEpoch(now.Add(90 * time.Minute)),
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
				"Anthropic-Ratelimit-Unified-7d-Reset":       soonestResetEpoch(now.Add(5 * 24 * time.Hour)),
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9",
			},
			wantOK:    true,
			wantReset: now.Add(90 * time.Minute),
			wantUsed:  25,
		},
		{
			name: "claude rejected window is skipped",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset":  soonestResetEpoch(now.Add(90 * time.Minute)),
				"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
				"Anthropic-Ratelimit-Unified-7d-Reset":  soonestResetEpoch(now.Add(5 * 24 * time.Hour)),
				"Anthropic-Ratelimit-Unified-7d-Status": "allowed",
			},
			wantOK:    true,
			wantReset: now.Add(5 * 24 * time.Hour),
			wantUsed:  -1,
		},
		{
			name: "codex additional per-model limits are ignored",
			signals: map[string]string{
				"X-Codex-Bengalfox-Primary-Reset-At":                      soonestResetEpoch(now.Add(10 * time.Minute)),
				"X-Codex-Additional-GPT-5.3-Codex-Spark-Primary-Reset-At": soonestResetEpoch(now.Add(10 * time.Minute)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{ID: "auth", Quota: QuotaState{ObservedAt: tt.observedAt, Signals: tt.signals}}
			gotReset, gotUsed, gotOK := nextQuotaReset(auth, now)
			if gotOK != tt.wantOK {
				t.Fatalf("nextQuotaReset() ok = %v, want %v (reset=%v used=%v)", gotOK, tt.wantOK, gotReset, gotUsed)
			}
			if !tt.wantOK {
				return
			}
			if !gotReset.Equal(tt.wantReset) {
				t.Fatalf("nextQuotaReset() reset = %v, want %v", gotReset, tt.wantReset)
			}
			if gotUsed != tt.wantUsed {
				t.Fatalf("nextQuotaReset() used = %v, want %v", gotUsed, tt.wantUsed)
			}
		})
	}
}

func TestSoonestResetOrder_SortsByResetMinuteThenUsageThenID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	withReset := func(id string, resetAt time.Time, usedPercent string) *Auth {
		return &Auth{ID: id, Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Reset-At":     soonestResetEpoch(resetAt),
			"X-Codex-Primary-Used-Percent": usedPercent,
		}}}
	}
	auths := []*Auth{
		withReset("later", now.Add(3*time.Hour), "95"),
		{ID: "unknown-b"},
		withReset("same-minute-low", now.Add(time.Hour+10*time.Second), "20"),
		withReset("same-minute-high-b", now.Add(time.Hour+40*time.Second), "80"),
		withReset("same-minute-high-a", now.Add(time.Hour+50*time.Second), "80"),
		withReset("next-minute", now.Add(time.Hour+70*time.Second), "99"),
		withReset("expired", now.Add(-time.Minute), "10"),
		{ID: "unknown-a"},
	}

	known, unknown := soonestResetOrder(auths, now)
	wantKnown := []string{"same-minute-high-a", "same-minute-high-b", "same-minute-low", "next-minute", "later"}
	wantUnknown := []string{"expired", "unknown-a", "unknown-b"}
	if len(known) != len(wantKnown) {
		t.Fatalf("known = %v, want %v", authIDs(known), wantKnown)
	}
	for i, id := range wantKnown {
		if known[i].ID != id {
			t.Fatalf("known = %v, want %v", authIDs(known), wantKnown)
		}
	}
	if len(unknown) != len(wantUnknown) {
		t.Fatalf("unknown = %v, want %v", authIDs(unknown), wantUnknown)
	}
	for i, id := range wantUnknown {
		if unknown[i].ID != id {
			t.Fatalf("unknown = %v, want %v", authIDs(unknown), wantUnknown)
		}
	}
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	return ids
}

func TestSoonestResetSelectorPick_PrefersSoonestReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		codexResetAuth("a-weekly", now.Add(72*time.Hour), "10"),
		codexResetAuth("b-soon", now.Add(2*time.Hour), "30"),
		{ID: "c-unknown", Provider: "codex"},
	}

	for i := 0; i < 3; i++ {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5.3-codex", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil || got.ID != "b-soon" {
			t.Fatalf("Pick() #%d auth = %v, want b-soon", i, got)
		}
	}
}

func TestSoonestResetSelectorPick_PriorityBuckets(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	highLate := codexResetAuth("high-late", now.Add(48*time.Hour), "10")
	highLate.Attributes = map[string]string{"priority": "10"}
	highSoon := codexResetAuth("high-soon", now.Add(5*time.Hour), "10")
	highSoon.Attributes = map[string]string{"priority": "10"}
	lowSoonest := codexResetAuth("low-soonest", now.Add(time.Hour), "90")
	lowSoonest.Attributes = map[string]string{"priority": "0"}

	got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{lowSoonest, highLate, highSoon})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "high-soon" {
		t.Fatalf("Pick() auth.ID = %q, want high-soon", got.ID)
	}

	highUnknownA := &Auth{ID: "high-unknown-a", Attributes: map[string]string{"priority": "10"}}
	highUnknownB := &Auth{ID: "high-unknown-b", Attributes: map[string]string{"priority": "10"}}
	want := []string{"high-unknown-a", "high-unknown-b", "high-unknown-a"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{lowSoonest, highUnknownB, highUnknownA})
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q (lower priority must not win on reset time)", i, got.ID, id)
		}
	}
}

func TestSoonestResetSelectorPick_UnknownAuthsRoundRobin(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	auths := []*Auth{
		{ID: "b"},
		codexResetAuth("c", now.Add(-time.Minute), "10"),
		{ID: "a", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Reset-At": "not-a-time"}}},
	}

	want := []string{"a", "b", "c", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5.3-codex", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

func TestSoonestResetSelectorPick_UnknownAuthsOnlyWhenKnownUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	known := codexResetAuth("known", now.Add(3*time.Hour), "10")
	auths := []*Auth{known, {ID: "unknown-a"}, {ID: "unknown-b"}}

	for i := 0; i < 3; i++ {
		got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != "known" {
			t.Fatalf("Pick() #%d auth.ID = %q, want known", i, got.ID)
		}
	}

	// The availability filter, not the ranking, removes a cooling-down credential.
	known.Unavailable = true
	known.NextRetryAfter = now.Add(30 * time.Minute)
	known.Quota.Exceeded = true
	known.Quota.NextRecoverAt = now.Add(30 * time.Minute)
	want := []string{"unknown-a", "unknown-b", "unknown-a"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() after cooldown #%d error = %v", i, err)
		}
		if got.ID != id {
			t.Fatalf("Pick() after cooldown #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

func TestSoonestResetSelectorPick_PrefersCodexWebsocketAuths(t *testing.T) {
	t.Parallel()

	now := time.Now()
	selector := &SoonestResetSelector{}
	httpSoon := codexResetAuth("http-soon", now.Add(time.Hour), "10")
	wsLate := codexResetAuth("ws-late", now.Add(24*time.Hour), "10")
	wsLate.Attributes = map[string]string{"websockets": "true"}
	auths := []*Auth{httpSoon, wsLate}

	got, err := selector.Pick(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() websocket error = %v", err)
	}
	if got.ID != "ws-late" {
		t.Fatalf("Pick() websocket auth.ID = %q, want ws-late", got.ID)
	}

	got, err = selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() http error = %v", err)
	}
	if got.ID != "http-soon" {
		t.Fatalf("Pick() http auth.ID = %q, want http-soon", got.ID)
	}
}

func TestSoonestResetSelectorPick_NoCandidates(t *testing.T) {
	t.Parallel()

	selector := &SoonestResetSelector{}
	if _, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil); err == nil {
		t.Fatal("Pick() without candidates returned no error")
	}
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	if got, err := selector.Pick(ctx, "codex", "", cliproxyexecutor.Options{}, []*Auth{nil}); err == nil || got != nil {
		t.Fatalf("Pick() with only nil candidates = %v, %v; want error", got, err)
	}
}

func TestSessionAffinitySelector_SoonestResetFallback(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()

	now := time.Now()
	sessionOpts := func(sessionID string) cliproxyexecutor.Options {
		headers := make(http.Header)
		headers.Set("Session_id", sessionID)
		return cliproxyexecutor.Options{Headers: headers}
	}
	pick := func(opts cliproxyexecutor.Options, auths []*Auth) string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "codex", "gpt-5.3-codex", opts, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return got.ID
	}

	auths := []*Auth{
		codexResetAuth("a", now.Add(4*time.Hour), "10"),
		codexResetAuth("b", now.Add(time.Hour), "10"),
	}
	if got := pick(cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("no-session Pick() = %q, want b from the soonest-reset fallback", got)
	}
	if got := pick(sessionOpts("srf-session-1"), auths); got != "b" {
		t.Fatalf("cold session Pick() = %q, want b", got)
	}

	// Account a now resets sooner. The established session keeps its binding while new
	// sessions and session-less requests follow the soonest reset.
	auths = []*Auth{
		codexResetAuth("a", now.Add(30*time.Minute), "10"),
		codexResetAuth("b", now.Add(time.Hour), "10"),
	}
	if got := pick(sessionOpts("srf-session-1"), auths); got != "b" {
		t.Fatalf("bound session Pick() = %q, want b", got)
	}
	if got := pick(sessionOpts("srf-session-2"), auths); got != "a" {
		t.Fatalf("new session Pick() = %q, want a", got)
	}
	if got := pick(cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("no-session Pick() = %q, want a", got)
	}

	// When the bound credential cools down, failover goes through the soonest-reset fallback.
	coolingB := codexResetAuth("b", now.Add(time.Hour), "10")
	coolingB.Unavailable = true
	coolingB.NextRetryAfter = now.Add(10 * time.Minute)
	auths = []*Auth{
		codexResetAuth("a", now.Add(30*time.Minute), "10"),
		coolingB,
		codexResetAuth("c", now.Add(2*time.Hour), "10"),
	}
	if got := pick(sessionOpts("srf-session-1"), auths); got != "a" {
		t.Fatalf("failover Pick() = %q, want a", got)
	}
}

type soonestResetTestExecutor struct {
	id string
}

func (e *soonestResetTestExecutor) Identifier() string { return e.id }

func (e *soonestResetTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *soonestResetTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "ExecuteStream not implemented"}
}

func (e *soonestResetTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *soonestResetTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *soonestResetTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func TestManagerSoonestResetSelectorRanksLiveQuotaSignals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&soonestResetTestExecutor{id: "codex"})
	if manager.useSchedulerFastPath() {
		t.Fatal("soonest-reset selection must not use the scheduler fast path")
	}
	if !isBuiltInSelector(manager.Selector()) {
		t.Fatal("soonest-reset must receive prevalidated candidates like the other built-in selectors")
	}

	now := time.Now()
	for _, auth := range []*Auth{
		codexResetAuth("srf-live-a", now.Add(3*time.Hour), "10"),
		codexResetAuth("srf-live-b", now.Add(time.Hour), "10"),
		{ID: "srf-live-c", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	// pickNext and pickNextMixed are the execution paths; SelectAuth always uses the legacy path.
	assertSelected := func(stage, want string) {
		t.Helper()
		picked, _, errPick := manager.pickNext(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("%s: pickNext() error = %v", stage, errPick)
		}
		if picked.ID != want {
			t.Fatalf("%s: pickNext() auth.ID = %q, want %q", stage, picked.ID, want)
		}
		mixed, _, _, errMixed := manager.pickNextMixed(ctx, []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
		if errMixed != nil {
			t.Fatalf("%s: pickNextMixed() error = %v", stage, errMixed)
		}
		if mixed.ID != want {
			t.Fatalf("%s: pickNextMixed() auth.ID = %q, want %q", stage, mixed.ID, want)
		}
		selected, errSelect := manager.SelectAuth(ctx, "codex", "", cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("%s: SelectAuth() error = %v", stage, errSelect)
		}
		if selected.ID != want {
			t.Fatalf("%s: SelectAuth() auth.ID = %q, want %q", stage, selected.ID, want)
		}
	}
	assertSelected("initial", "srf-live-b")

	// A response observed on srf-live-a reports a sooner reset, which the next selection must use.
	headerCtx := internallogging.WithResponseHeadersHolder(ctx)
	internallogging.SetResponseHeaders(headerCtx, http.Header{
		"X-Codex-Primary-Used-Percent":   []string{"55"},
		"X-Codex-Primary-Window-Minutes": []string{"300"},
		"X-Codex-Primary-Reset-At":       []string{soonestResetEpoch(now.Add(20 * time.Minute))},
	})
	manager.MarkResult(headerCtx, Result{AuthID: "srf-live-a", Provider: "codex", Model: "gpt-5.3-codex", Success: true})
	assertSelected("after observation", "srf-live-a")
}

func TestManagerSoonestResetMixedPrefersCodexWebsocket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&soonestResetTestExecutor{id: "codex"})
	if manager.useSchedulerFastPath() {
		t.Fatal("expected legacy mixed selection path")
	}
	now := time.Now()
	httpSoon := codexResetAuth("http-soon", now.Add(time.Hour), "10")
	wsLate := codexResetAuth("ws-late", now.Add(24*time.Hour), "10")
	wsLate.Attributes = map[string]string{"websockets": "true"}
	for _, auth := range []*Auth{httpSoon, wsLate} {
		if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}

	wsCtx := cliproxyexecutor.WithDownstreamWebsocket(ctx)
	picked, _, provider, err := manager.pickNextMixed(wsCtx, []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickNextMixed: %v", err)
	}
	if picked.ID != "ws-late" || provider != "codex" {
		t.Fatalf("pickNextMixed websocket = (%q, %q), want (ws-late, codex)", picked.ID, provider)
	}
}

func TestManagerSoonestResetMixedWebsocketBehavior(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		websocket  bool
		other      bool
		triedOther bool
		wsEnabled  bool
		wsPriority string
		want       string
	}{
		{name: "http keeps reset ordering", wsEnabled: true, want: "codex-http"},
		{name: "no websocket capable auth falls back", websocket: true, want: "codex-http"},
		{name: "mixed providers preserve reset ordering", websocket: true, other: true, wsEnabled: true, want: "other-soon"},
		{name: "tried other provider is excluded", websocket: true, other: true, triedOther: true, wsEnabled: true, want: "codex-ws"},
		{name: "higher priority precedes websocket preference", websocket: true, wsEnabled: true, wsPriority: "-1", want: "codex-http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			manager := NewManager(nil, &SoonestResetSelector{}, nil)
			manager.RegisterExecutor(&soonestResetTestExecutor{id: "codex"})
			manager.RegisterExecutor(&soonestResetTestExecutor{id: "claude"})
			now := time.Now()
			httpAuth := codexResetAuth("codex-http", now.Add(time.Hour), "10")
			wsAuth := codexResetAuth("codex-ws", now.Add(24*time.Hour), "10")
			wsAuth.Attributes = map[string]string{"websockets": strconv.FormatBool(tt.wsEnabled), "priority": tt.wsPriority}
			auths := []*Auth{httpAuth, wsAuth}
			if tt.other {
				other := &Auth{ID: "other-soon", Provider: "claude", Quota: QuotaState{
					Signals: map[string]string{"anthropic-ratelimit-unified-5h-reset": soonestResetEpoch(now.Add(30 * time.Minute))},
				}}
				auths = append(auths, other)
			}
			for _, auth := range auths {
				if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
					t.Fatalf("Register(%s): %v", auth.ID, err)
				}
			}
			if tt.websocket {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			}
			tried := map[string]struct{}{}
			if tt.triedOther {
				tried["other-soon"] = struct{}{}
			}
			picked, _, _, err := manager.pickNextMixed(ctx, []string{"codex", "claude"}, "", cliproxyexecutor.Options{}, tried)
			if err != nil {
				t.Fatalf("pickNextMixed: %v", err)
			}
			if picked.ID != tt.want {
				t.Fatalf("pickNextMixed = %q, want %q", picked.ID, tt.want)
			}
		})
	}
}

func TestManagerSoonestResetMixedWebsocketSessionAffinity(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}, TTL: time.Minute})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	if manager.useSchedulerFastPath() {
		t.Fatal("expected legacy mixed selection path")
	}
	manager.RegisterExecutor(&soonestResetTestExecutor{id: "codex"})
	ctx := context.Background()
	now := time.Now()
	httpAuth := codexResetAuth("http-soon", now.Add(time.Hour), "10")
	wsAuth := codexResetAuth("ws-late", now.Add(24*time.Hour), "10")
	wsAuth.Attributes = map[string]string{"websockets": "true"}
	for _, auth := range []*Auth{httpAuth, wsAuth} {
		if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	pick := func(ctx context.Context, session string) string {
		t.Helper()
		headers := make(http.Header)
		headers.Set("Session_id", session)
		picked, _, _, err := manager.pickNextMixed(ctx, []string{"codex"}, "", cliproxyexecutor.Options{Headers: headers}, nil)
		if err != nil {
			t.Fatalf("pickNextMixed: %v", err)
		}
		return picked.ID
	}
	wsCtx := cliproxyexecutor.WithDownstreamWebsocket(ctx)
	if got := pick(wsCtx, "new-ws-session"); got != "ws-late" {
		t.Fatalf("cold websocket session = %q, want ws-late", got)
	}
	if got := pick(ctx, "existing-http-session"); got != "http-soon" {
		t.Fatalf("cold HTTP session = %q, want http-soon", got)
	}
	if got := pick(wsCtx, "existing-http-session"); got != "http-soon" {
		t.Fatalf("bound websocket session = %q, want existing binding http-soon", got)
	}
}
