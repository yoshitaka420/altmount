package api

// Provider health states surfaced in ProviderStatusResponse.State. They are
// derived from live nntppool stats by classifyProviderState and rendered as a
// colored badge in the frontend ProviderCard.
const (
	providerStateHealthy       = "healthy"
	providerStateDegraded      = "degraded"
	providerStateUnreachable   = "unreachable"
	providerStateQuotaExceeded = "quota_exceeded"
)

// classifyProviderState derives a human-facing health state (and, when
// unhealthy, a short failure reason) for one provider from its live pool stats.
//
//   - unreachable: the most recent DATE ping failed and nothing is actively
//     connected, so the backend genuinely can't be reached right now.
//   - quota_exceeded: reachable but unusable until the quota period resets.
//   - degraded: reachable but unhealthy — a high missing-article rate, or pings
//     erroring while connections are still serving traffic.
//   - healthy: everything else.
//
// pingErr is the last DATE ping error (nil = ok), activeConnections is the
// number of currently-running connections, and quotaExceeded / missingWarning
// are the pool and metrics-tracker flags respectively.
func classifyProviderState(pingErr error, activeConnections int, quotaExceeded, missingWarning bool) (state, failureReason string) {
	if pingErr != nil && activeConnections == 0 {
		return providerStateUnreachable, pingErr.Error()
	}
	if quotaExceeded {
		return providerStateQuotaExceeded, "download quota exceeded"
	}
	if missingWarning {
		return providerStateDegraded, "high missing-article rate"
	}
	if pingErr != nil {
		return providerStateDegraded, pingErr.Error()
	}
	return providerStateHealthy, ""
}
