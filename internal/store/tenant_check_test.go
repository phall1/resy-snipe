//go:build tenancy

// Package store: tenancy enforcement harness.
//
// This test enforces that every Store method either takes a homelab
// domain.UserID parameter or is on the v1-legacy / cross-tenant
// allowlist in tenant_allowlist.go. It is the build-time gate
// described in docs/v2/design/multi-user.md §tenancy-enforcement
// (acceptance criterion A5 of milestone M1).
//
// Run it explicitly:
//
//	go test -race -tags=tenancy ./internal/store/...
//
// Or via `just test-tenancy`. The file is invisible without the
// `tenancy` build tag so the default `go test ./...` run still
// produces a green tree even while the v2 Store methods are being
// designed.
package store

import (
	"reflect"
	"strings"
	"testing"

	"resy-snipe/internal/domain"
)

// domainUserIDType is the reflect.Type of domain.UserID. The harness
// compares parameter types against this to decide whether a method
// satisfies the tenancy contract. We compare by exact Type identity
// (not AssignableTo) because plain `string` assigns to
// `domain.UserID` and the whole point of the named type is to make
// the tenancy parameter syntactically obvious in signatures.
var domainUserIDType = reflect.TypeFor[domain.UserID]()

// TestStoreMethodsTakeUserID asserts every method on the Store
// interface either takes a domain.UserID argument or is named in
// v1LegacyMethodAllowlist. A new Store method that omits userID and
// is not allowlisted fails this test.
//
// Failure points at docs/v2/design/multi-user.md
// §tenancy-enforcement.
func TestStoreMethodsTakeUserID(t *testing.T) {
	t.Parallel()
	storeType := reflect.TypeFor[Store]()
	if storeType.Kind() != reflect.Interface {
		t.Fatalf("Store is not an interface kind: got %s", storeType.Kind())
	}

	allow := make(map[string]struct{}, len(v1LegacyMethodAllowlist))
	for _, name := range v1LegacyMethodAllowlist {
		allow[name] = struct{}{}
	}

	type v2Method struct {
		name string
		mtyp reflect.Type
	}
	var v2Methods []v2Method
	var violations []string

	for i := range storeType.NumMethod() {
		m := storeType.Method(i)
		if _, ok := allow[m.Name]; ok {
			// Allowlisted v1-legacy or cross-tenant method.
			continue
		}
		// v2 method: walk its parameters looking for domain.UserID.
		if !methodTakesUserID(m.Type) {
			violations = append(violations, m.Name+m.Type.String())
			continue
		}
		v2Methods = append(v2Methods, v2Method{name: m.Name, mtyp: m.Type})
	}

	if len(violations) > 0 {
		t.Fatalf(
			"tenancy harness: %d Store method(s) violate the v2 tenancy contract.\n"+
				"Each v2 Store method must take a `userID domain.UserID` parameter,\n"+
				"or be added to v1LegacyMethodAllowlist in tenant_allowlist.go with a comment\n"+
				"explaining why it is exempt. See docs/v2/design/multi-user.md §tenancy-enforcement.\n"+
				"Offending methods:\n  - %s",
			len(violations),
			strings.Join(violations, "\n  - "),
		)
	}

	t.Logf("tenancy harness: %d v2 Store method(s) checked, all take domain.UserID.", len(v2Methods))
	t.Logf("tenancy harness: %d v1-legacy / cross-tenant method(s) allowlisted.", len(allow))
}

// methodTakesUserID reports whether mt's parameter list contains a
// domain.UserID argument. mt is the reflect.Type of an interface
// method, so its first In() parameter is NOT the receiver (interface
// method types are pre-stripped of the receiver).
func methodTakesUserID(mt reflect.Type) bool {
	if mt.Kind() != reflect.Func {
		return false
	}
	for i := range mt.NumIn() {
		if mt.In(i) == domainUserIDType {
			return true
		}
	}
	return false
}

// TestStoreTenancyIsolation is the live cross-tenant isolation check.
// For every v2 Store method that takes a domain.UserID, the test
// seeds two users into an in-memory SQLite database, calls the
// method with each user's ID, and asserts neither call returns the
// other user's rows.
//
// Today the Store interface has zero v2 methods (the v1 bridge is
// the only API surface). The sub-test therefore short-circuits with
// t.Skip; it will activate automatically once M1-10/M1-11 land
// quest/audit Store methods.
func TestStoreTenancyIsolation(t *testing.T) {
	t.Parallel()
	storeType := reflect.TypeFor[Store]()
	allow := make(map[string]struct{}, len(v1LegacyMethodAllowlist))
	for _, name := range v1LegacyMethodAllowlist {
		allow[name] = struct{}{}
	}

	var v2Names []string
	for i := range storeType.NumMethod() {
		m := storeType.Method(i)
		if _, ok := allow[m.Name]; ok {
			continue
		}
		v2Names = append(v2Names, m.Name)
	}

	if len(v2Names) == 0 {
		t.Skip("no v2 Store methods yet — see M1-10/M1-11 etc.")
	}

	// When v2 methods land, replace this stub with a real isolation
	// run: open two SQLiteStores against a shared in-memory DB,
	// seed usr_a and usr_b, invoke each v2 method with both IDs,
	// assert the row sets are disjoint. The harness intentionally
	// does NOT auto-construct call arguments today because no v2
	// methods exist to model. Make the failure loud so the operator
	// who adds the first v2 method also wires up real isolation
	// fixtures.
	t.Fatalf(
		"tenancy harness: %d v2 Store method(s) detected (%s) but no live isolation fixtures yet.\n"+
			"Extend TestStoreTenancyIsolation to seed two users and call each method\n"+
			"with mismatched user IDs. See docs/v2/design/multi-user.md §tenancy-enforcement.",
		len(v2Names),
		strings.Join(v2Names, ", "),
	)
}
