package arrs

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/javi11/altmount/internal/arrs/model"
)

// IsUnreachableError reports whether err indicates the arr instance could not be
// reached at the transport level — DNS failure (no such host), connection
// refused/reset, timeout, no route to host, TLS handshake timeout. These are
// transient outages, not a verdict on the file, so callers should DEFER repair
// rather than condemn the file.
//
// Logical arr responses are explicitly NOT treated as unreachable: a path that
// didn't match (ErrPathMatchFailed), an already-satisfied episode
// (ErrEpisodeAlreadySatisfied), a missing/disabled instance (ErrInstanceNotFound)
// and any HTTP error carrying a real response body keep the existing behavior.
func IsUnreachableError(err error) bool {
	if err == nil {
		return false
	}

	// Logical/business outcomes are never "unreachable" — preserve current handling.
	if errors.Is(err, model.ErrPathMatchFailed) ||
		errors.Is(err, model.ErrEpisodeAlreadySatisfied) ||
		errors.Is(err, model.ErrInstanceNotFound) {
		return false
	}

	// Timeouts and cancellation: a context deadline, an explicit cancel, or any
	// net.Error reporting a timeout. A canceled context (e.g. the worker shutting
	// down mid-repair) is not a verdict on the file, so callers should DEFER repair
	// rather than condemn it.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// DNS resolution failure (e.g. "no such host", "server misbehaving").
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Connection-level failures.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}

	// A *net.OpError (dial/read/write at the socket layer) is a transport failure.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// http.Client failures surface as *url.Error; a timeout there is transport.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}

	// Conservative string fallback for transport phrases that don't always reach
	// us as typed errors through the starr/http stack. NOTE: this matches on error
	// message text, so upstream formatting changes (starr/net/http) can silently
	// shift classification — extend this list cautiously and prefer typed checks
	// above whenever the error kind is available.
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"no such host",
		"connection refused",
		"connection reset",
		"no route to host",
		"network is unreachable",
		"i/o timeout",
		"tls handshake timeout",
		"context deadline exceeded",
		"server misbehaving",
		"dial tcp",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}
