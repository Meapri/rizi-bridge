# Rizi Bridge fork topics

Rizi Bridge carries a small set of fork-only changes on top of upstream
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Each topic lives on its own
`stack/*` branch so it can be rebased, dropped, or proposed upstream on its own; `main`
integrates the stacks. The Go module path stays `github.com/router-for-me/CLIProxyAPI/v8`
so upstream syncs stay conflict-free.

| Topic | Branch | Kind |
| --- | --- | --- |
| [Rebrand](#rebrand) | `stack/rebrand` | Docs and example config |
| [Default image main model](#default-image-main-model) | `stack/default-image-main-model` | Behavior default |
| [Soonest-reset routing](#soonest-reset-routing) | `stack/soonest-reset-routing` | New routing strategy |

## Rebrand

Commit `a9751481` (`docs: rebrand fork as Rizi Bridge`).

- Adds the fork header to `README.md` and names the binary `rizi-bridge`
  (`go build -o rizi-bridge ./cmd/server`).
- Uses `RiziBridge` as the example Sign in with ChatGPT (SIWC) identifier in
  `config.example.yaml` (`signin-with-chatgpt-identifier`).
- Sync note: upstream edits to the top of `README.md` conflict with the fork header.

## Default image main model

Commit `1b762f5b` (`feat(images): default image main model to gpt-6.1-sol`).

- Changes the default main model of the images endpoints from `gpt-5.4-mini` to
  `gpt-6.1-sol`, because `gpt-5.4-mini` is rejected (`model_not_found`) for SIWC Codex
  requests. `multimedia.gpt-image-2-base-model` still overrides the default.
- Files: `internal/runtime/executor/codex_openai_images.go`,
  `sdk/api/handlers/openai/openai_images_handlers.go`, `internal/config/sdk_config.go`
  (comment), and `config.example.yaml` (comment).
- Sync note: recheck both constants whenever upstream changes its image model defaults.

## Soonest-reset routing

Adds `routing.strategy: "soonest-reset"` (aliases `soonest-reset-first` and `srf`). When
several credentials can serve a request, it uses the credential whose quota resets
soonest, so quota is spent before it expires unused at the reset.

Behavior:

- Candidates are narrowed like the other built-in strategies: the highest available
  priority tier, with Codex websocket-capable credentials preferred for downstream
  websocket requests. Cooldown and quota-exceeded credentials are excluded by the shared
  availability filter.
- The reset time comes from the passive quota snapshot in `Auth.Quota.Signals`: Codex
  `x-codex-{primary,secondary}-reset-at` / `-reset-after-seconds` (relative to
  `Quota.ObservedAt`) with `-used-percent`, Claude
  `anthropic-ratelimit-unified-{5h,7d}-reset` with `-utilization` / `-status`, and Devin
  `{daily,weekly}_quota_reset_at` with `*_remaining_percent`. Codex additional per-model
  limits are ignored.
- Among a credential's windows that still have headroom, the earliest reset counts.
  Resets that already passed, lie more than 8 days ahead, or fail to parse count as
  unknown.
- Credentials with a known reset are ordered by reset minute, then by higher used
  percent, then by ID. Credentials without a known reset are used only when no
  credential with a known reset is available, and rotate round-robin among themselves.
- With `routing.session-affinity: true`, the strategy is the affinity fallback: it picks
  the credential for new bindings, session-less requests, and failover, while established
  bindings stay put.
- The selection runs on the manager's legacy selection path, which reads live quota
  signals, instead of the scheduler fast path, which has no soonest-reset strategy.
- Quota signals live in memory only. After a restart every credential is unknown until it
  serves a request. In Home mode, selection is delegated to Home and this strategy does
  not apply.

Files:

- New: `sdk/cliproxy/auth/selector_soonest_reset.go` and its test,
  `sdk/cliproxy/service_config_soonest_reset_test.go`,
  `internal/api/handlers/management/config_basic_routing_strategy_test.go`.
- Small edits to upstream files: `sdk/cliproxy/auth/conductor_selection.go`
  (`isBuiltInSelector`, `useSchedulerFastPath`), `sdk/cliproxy/service_config.go`
  (strategy parsing), `internal/api/handlers/management/config_basic.go` (v0
  `PUT /routing/strategy` validation), `internal/config/config_types.go` (comment), and
  `config.example.yaml` (comment).

Sync notes:

- If upstream adds scheduler strategies or changes `isBuiltInSelector`,
  `useSchedulerFastPath`, or `selectorStrategy`, confirm that `SoonestResetSelector` still
  receives prevalidated candidates and still skips the scheduler fast path.
- The strategy name matches the opt-in `soonest-reset` strategy of the
  [cliproxy-rs](https://github.com/vayungodara/cliproxy-rs) port, which shares the same
  `config.yaml`. That port is described as ranking by the weekly window only, so the two
  implementations may order credentials differently.
