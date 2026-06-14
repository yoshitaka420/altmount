package api

import (
	"errors"
	"testing"
)

// TestClassifyProviderState pins the per-provider health classification used to
// drive the UI badge: precedence is unreachable > quota_exceeded > degraded >
// healthy, and an unhealthy verdict always carries a failure reason.
func TestClassifyProviderState(t *testing.T) {
	t.Parallel()

	pingErr := errors.New("dial tcp: connection refused")

	cases := []struct {
		name              string
		pingErr           error
		activeConnections int
		quotaExceeded     bool
		missingWarning    bool
		wantState         string
		wantReasonEmpty   bool
	}{
		{
			name:      "healthy when everything is fine",
			wantState: providerStateHealthy, wantReasonEmpty: true,
		},
		{
			name:    "unreachable when ping fails and nothing is connected",
			pingErr: pingErr, activeConnections: 0,
			wantState: providerStateUnreachable,
		},
		{
			name:    "ping error while serving is degraded, not unreachable",
			pingErr: pingErr, activeConnections: 4,
			wantState: providerStateDegraded,
		},
		{
			name:          "quota exceeded outranks degraded signals",
			quotaExceeded: true, missingWarning: true,
			wantState: providerStateQuotaExceeded,
		},
		{
			name:           "high missing rate is degraded",
			missingWarning: true,
			wantState:      providerStateDegraded,
		},
		{
			name:    "unreachable outranks quota when both apply",
			pingErr: pingErr, activeConnections: 0, quotaExceeded: true,
			wantState: providerStateUnreachable,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, reason := classifyProviderState(
				tc.pingErr, tc.activeConnections, tc.quotaExceeded, tc.missingWarning,
			)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if tc.wantReasonEmpty && reason != "" {
				t.Errorf("reason = %q, want empty", reason)
			}
			if !tc.wantReasonEmpty && state != providerStateHealthy && reason == "" {
				t.Error("unhealthy state must carry a failure reason")
			}
		})
	}
}
