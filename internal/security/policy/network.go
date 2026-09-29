package policy

type NetworkApprovalMode string

const (
	NetworkImmediate NetworkApprovalMode = "immediate"
	NetworkDeferred  NetworkApprovalMode = "deferred"
)
