package tool

import "github.com/fwtllh-png/QCode/internal/security/netpolicy"

func HostResource(target netpolicy.Target, access AccessMode) Resource {
	return Resource{
		Kind: "host", ID: target.Host, Access: access,
		Protocol: target.Scheme, Port: target.Port,
	}
}
