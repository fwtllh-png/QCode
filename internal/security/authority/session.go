package authority

// SessionBinding is prepared by the trusted command adapter before a lease is
// issued. The broker checks its concrete command against CommandDigest.
type SessionBinding struct {
	CommandDigest string
	Value         any
}

type AuthorizedSessionGrant struct {
	Lease      ExecutionLease
	Validation LeaseValidation
	Prepared   any
}
