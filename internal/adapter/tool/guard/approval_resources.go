package guard

import (
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// approvalResources keeps the Web canonical resource shape independent from
// security's typed snapshot. It is a display projection only.
func approvalResources(resources []securitymodel.Resource) []tool.Resource {
	result := make([]tool.Resource, 0, len(resources))
	for _, item := range resources {
		out := tool.Resource{Kind: item.Class.String(), ID: item.ID, Access: item.Access, Tree: item.Tree,
			Methods: append([]string(nil), item.Methods...), AllowPrivate: item.AllowPrivate}
		switch item.Class {
		case securitymodel.ClassPath:
			out.Kind, out.Path, out.ID = securitymodel.KindFile, item.Path, ""
			if item.Tree {
				out.Kind = securitymodel.KindDirectory
			}
		case securitymodel.ClassNetwork:
			out.Kind = securitymodel.KindHost
			if item.Network != nil {
				out.ID, out.Protocol, out.Port = item.Network.Host, item.Network.Scheme, item.Network.Port
			}
			if item.URL != "" {
				out.Kind, out.ID = securitymodel.KindURL, item.URL
			}
		case securitymodel.ClassLoopback:
			out.Kind, out.ID, out.Protocol = securitymodel.KindHost, securitymodel.LoopbackHost, securitymodel.LoopbackProtocol
		case securitymodel.ClassNamed:
			out.Kind = item.Name
		}
		result = append(result, out)
	}
	return result
}
