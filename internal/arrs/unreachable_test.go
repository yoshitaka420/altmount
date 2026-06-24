package arrs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"

	"github.com/javi11/altmount/internal/arrs/model"
)

// TestIsUnreachableError pins the transport-vs-logical classification that decides
// whether a repair failure DEFERS (arr unreachable) or CONDEMNS (genuine failure).
func TestIsUnreachableError(t *testing.T) {
	dns := &net.DNSError{Err: "no such host", Name: "sonarr.local", IsNotFound: true}
	connRefused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

	unreachable := []struct {
		name string
		err  error
	}{
		{"dns no such host", dns},
		{"dns wrapped by fmt", fmt.Errorf("failed to get episodes for series X: %w", dns)},
		{"dns wrapped by url.Error", &url.Error{Op: "Get", URL: "http://sonarr.local/api", Err: dns}},
		{"connection refused (OpError)", connRefused},
		{"connection refused (syscall)", fmt.Errorf("dial: %w", syscall.ECONNREFUSED)},
		{"context deadline exceeded", context.DeadlineExceeded},
		{"string: connection refused", errors.New("dial tcp 10.0.0.5:8989: connect: connection refused")},
		{"string: i/o timeout", errors.New("Get \"http://x\": net/http: request canceled (i/o timeout)")},
		{"string: tls handshake timeout", errors.New("net/http: TLS handshake timeout")},
	}
	for _, tt := range unreachable {
		t.Run("unreachable/"+tt.name, func(t *testing.T) {
			if !IsUnreachableError(tt.err) {
				t.Errorf("IsUnreachableError(%v) = false; want true", tt.err)
			}
		})
	}

	logical := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"path match failed", model.ErrPathMatchFailed},
		{"path match failed wrapped", fmt.Errorf("no series found: %w", model.ErrPathMatchFailed)},
		{"episode already satisfied", model.ErrEpisodeAlreadySatisfied},
		{"instance not found", model.ErrInstanceNotFound},
		{"http 500 body", errors.New("sonarr returned 500: internal server error")},
		{"movie not found (logical)", errors.New("no movie found with file path in library")},
	}
	for _, tt := range logical {
		t.Run("logical/"+tt.name, func(t *testing.T) {
			if IsUnreachableError(tt.err) {
				t.Errorf("IsUnreachableError(%v) = true; want false (logical/genuine failure must still condemn)", tt.err)
			}
		})
	}
}

// TestUnreachableNotMisclassifiedAsLogical guards the precedence: a transport error
// must win even if its message vaguely resembles a logical one.
func TestIsUnreachableError_PrecedenceTransportWins(t *testing.T) {
	// A DNS failure whose surrounding message mentions a series lookup must still
	// be unreachable, not condemned.
	err := fmt.Errorf("failed to get episodes for series Foo: %w",
		&net.DNSError{Err: "server misbehaving", Name: "sonarr"})
	if !IsUnreachableError(err) {
		t.Errorf("transport error wrapped in a logical-sounding message must be unreachable")
	}
}
