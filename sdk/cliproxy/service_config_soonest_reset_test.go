package cliproxy

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, strategy := range []string{"soonest-reset", "soonest-reset-first", "srf", " Soonest-Reset "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: strategy},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy %q normalized to %q, want soonest-reset", strategy, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
		}
	}
}

func TestSoonestResetRoutingSelectorWithSessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true},
	})
	affinity, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
	defer affinity.Stop()

	// Round-robin would start at "a"; only the soonest-reset fallback starts at "b".
	now := time.Now()
	resetSignals := func(resetAt time.Time) coreauth.QuotaState {
		return coreauth.QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Reset-At": strconv.FormatInt(resetAt.Unix(), 10),
		}}
	}
	auths := []*coreauth.Auth{
		{ID: "a", Quota: resetSignals(now.Add(5 * time.Hour))},
		{ID: "b", Quota: resetSignals(now.Add(time.Hour))},
	}
	got, errPick := affinity.Pick(context.Background(), "codex", "gpt-5.3-codex", cliproxyexecutor.Options{}, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want b from the soonest-reset fallback", got.ID)
	}
}
