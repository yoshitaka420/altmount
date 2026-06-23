package triage

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/arrs/model"
)

// TestEvaluate_DeadAndHidden_RoutesThroughOwnershipGate verifies that once a
// record is treated as existing (the fix makes a corrupted_metadata copy count
// as existing), Evaluate routes the decision through the ownership gate rather
// than file_removed. It pins all four outcomes, including the critical
// fail-closed case:
//
//	a clean Unowned/Replaced determination -> DELETE
//	Owned -> KEEP
//	Unknown (lookup ERROR/TIMEOUT, or arr unreachable) -> KEEP, never delete
//
// A failed lookup surfaces to triage as model.OwnershipUnknown (ResolveOwnership
// is fail-closed: see internal/arrs/scanner/ownership_dispatch.go), so it must
// never become "unowned -> delete".
func TestEvaluate_DeadAndHidden_RoutesThroughOwnershipGate(t *testing.T) {
	const path = "/a.mkv"

	newService := func(own model.Ownership) *Service {
		store := &fakeStore{}
		// exists=true models "original .meta present" OR "corrupted_metadata copy
		// present" — both of which the (fixed) Exists() reports as existing.
		meta := &fakeMeta{exists: map[string]bool{path: true}}
		res := &fakeResolver{byPath: map[string]model.Ownership{path: own}}
		return NewService(cfgGetter(true, 50, 500), store, meta, res)
	}

	cases := []struct {
		name       string
		own        model.Ownership
		wantAction Action
		wantReason Reason
	}{
		{
			name:       "clean unowned -> delete",
			own:        model.Ownership{Status: model.OwnershipUnowned, InstanceType: "sonarr", InstanceName: "main"},
			wantAction: ActionDelete,
			wantReason: ReasonDeadUnowned,
		},
		{
			name:       "clean replaced -> delete",
			own:        model.Ownership{Status: model.OwnershipReplaced, InstanceType: "sonarr", InstanceName: "main", ReplacementID: 42},
			wantAction: ActionDelete,
			wantReason: ReasonDeadReplaced,
		},
		{
			name:       "owned -> keep",
			own:        model.Ownership{Status: model.OwnershipOwned, InstanceType: "sonarr", InstanceName: "main"},
			wantAction: ActionKeep,
			wantReason: ReasonOwned,
		},
		{
			// arr was reached (instance resolved) but the lookup itself errored or
			// timed out -> fail-closed Unknown. MUST be kept, never deleted.
			name:       "lookup error/timeout (reachable but failed) -> keep, NOT unowned",
			own:        model.Ownership{Status: model.OwnershipUnknown, InstanceType: "sonarr", InstanceName: "main"},
			wantAction: ActionKeep,
			wantReason: ReasonUnknown,
		},
		{
			// arr entirely unreachable / none configured -> zero-value Unknown. Keep.
			name:       "arr unreachable / unknown -> keep",
			own:        model.Ownership{Status: model.OwnershipUnknown},
			wantAction: ActionKeep,
			wantReason: ReasonUnknown,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := newService(c.own).Evaluate(context.Background(), corruptedItem(path))
			if dec.Action != c.wantAction || dec.Reason != c.wantReason {
				t.Fatalf("Evaluate = {action:%v reason:%v}; want {action:%v reason:%v}",
					dec.Action, dec.Reason, c.wantAction, c.wantReason)
			}
		})
	}
}

// TestProcessItem_DeadAndHidden_LookupErrorKeptEndToEnd drives the full
// ProcessItem path (not just Evaluate) for the fail-closed case to prove no
// delete is attempted on the health store when ownership is Unknown.
func TestProcessItem_DeadAndHidden_LookupErrorKeptEndToEnd(t *testing.T) {
	const path = "/a.mkv"
	store := &fakeStore{}
	meta := &fakeMeta{exists: map[string]bool{path: true}}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		path: {Status: model.OwnershipUnknown, InstanceType: "sonarr", InstanceName: "main"},
	}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	if deleted := svc.ProcessItem(context.Background(), corruptedItem(path), SourceEnterCorrupted); deleted {
		t.Fatalf("ProcessItem deleted a record whose ownership lookup failed (Unknown); must keep")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("health record delete attempted on fail-closed Unknown: %v", store.deleted)
	}
	if len(meta.deleted) != 0 {
		t.Fatalf(".meta delete attempted on fail-closed Unknown: %v", meta.deleted)
	}
}
