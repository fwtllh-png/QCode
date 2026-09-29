package policy

import (
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

type NetworkApprovalMode string

const (
	NetworkImmediate NetworkApprovalMode = "immediate"
	NetworkDeferred  NetworkApprovalMode = "deferred"
)

func targetsHostLocal(resources []tool.Resource) bool {
	for _, resource := range resources {
		switch resource.Kind {
		case "host":
			// A loopback grant opens every local port to the sandboxed
			// process, including other sessions' proxy channels and host
			// services, so it is host-local like any localhost target.
			if netpolicy.NamesHostLocal(resource.ID) {
				return true
			}
		case "url":
			if target, err := netpolicy.ParseTarget(resource.ID); err == nil &&
				netpolicy.NamesHostLocal(target.Host) {
				return true
			}
		}
	}
	return false
}

func HostResource(target netpolicy.Target, access tool.AccessMode) tool.Resource {
	return tool.Resource{
		Kind: "host", ID: target.Host, Access: access,
		Protocol: target.Scheme, Port: target.Port,
	}
}
