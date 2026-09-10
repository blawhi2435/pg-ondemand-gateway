// Package authz implements seam B: the authorization check called after
// StartupMessage parsing and before dialing the backend (design §5, §6
// step 6). Phase 1 ships only allowAll; phase 2 replaces the implementation
// behind this same interface without moving the call site.
package authz

// Decision is the result of an authorization check.
type Decision struct {
	Allow    bool
	SQLState string // set when Allow is false
	Reason   string
}

// Authorizer is seam B.
type Authorizer interface {
	Check(cluster, user, database, clientIP string) Decision
}

// AllowAll is the phase 1 Authorizer: it always allows, without consulting
// any external system. The call site it plugs into is already positioned
// where phase 2's real check must run (after StartupMessage, before dial).
type AllowAll struct{}

// NewAllowAll returns an Authorizer that always allows.
func NewAllowAll() AllowAll {
	return AllowAll{}
}

// Check implements Authorizer.
func (AllowAll) Check(cluster, user, database, clientIP string) Decision {
	return Decision{Allow: true}
}
