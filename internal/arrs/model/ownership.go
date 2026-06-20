package model

// OwnershipStatus describes whether an *arr currently owns a given file. The
// zero value is intentionally OwnershipUnknown so that any path which fails to
// make a positive determination is fail-closed: callers must never treat
// "unknown" as "unowned".
type OwnershipStatus int

const (
	// OwnershipUnknown means ownership could not be determined (a lookup
	// errored/timed out, the managing instance could not be reached, or the
	// arr is not introspectable for fine-grained file ownership). Callers MUST
	// treat this as "do not delete".
	OwnershipUnknown OwnershipStatus = iota
	// OwnershipOwned means an arr tracks this title/file and is expected to
	// repair or re-grab it. The file must be kept.
	OwnershipOwned
	// OwnershipUnowned means no arr tracks this file, so no repair will ever
	// arrive. It is safe to remove the bookkeeping for it.
	OwnershipUnowned
	// OwnershipReplaced means the arr already has a *different* healthy file
	// for this title (an upgrade/replacement). The old copy is redundant and
	// safe to remove.
	OwnershipReplaced
)

// String renders the status for logs.
func (s OwnershipStatus) String() string {
	switch s {
	case OwnershipOwned:
		return "owned"
	case OwnershipUnowned:
		return "unowned"
	case OwnershipReplaced:
		return "replaced"
	default:
		return "unknown"
	}
}

// Ownership is the result of resolving who owns a file. It is always safe to
// inspect even on error: the zero value reports OwnershipUnknown.
type Ownership struct {
	Status        OwnershipStatus
	InstanceType  string
	InstanceName  string
	ReplacementID int64 // the new file id when Status == OwnershipReplaced (0 if not known)
}

// SafeToDelete reports whether the bookkeeping for this file may be removed.
// It is true only for a positively determined Unowned or Replaced status; it is
// false for Owned and (critically) for Unknown.
func (o Ownership) SafeToDelete() bool {
	return o.Status == OwnershipUnowned || o.Status == OwnershipReplaced
}
