package environment

import (
	_ "embed"
	"encoding/json"
)

//go:embed testdata/eds-go-module-info.json
var edsGoModuleInfoJSON []byte

func EDSGoModuleInfoSpec() (EnvironmentSpec, error) {
	var spec EnvironmentSpec
	if err := json.Unmarshal(edsGoModuleInfoJSON, &spec); err != nil {
		return EnvironmentSpec{}, err
	}
	return spec, nil
}

func EDSGoBaselineObservations() []BaselineObservation {
	return []BaselineObservation{
		{
			ID:             "missing_host_go_env",
			Category:       CategoryEnvironmentResourceUnavailable,
			RequiredAction: ActionApproveHostConfig,
			ForbiddenClaim: "host has no GOPROXY",
		},
		{
			ID:             "goproxy_auth_failed",
			Category:       CategoryCredentialUnavailable,
			RequiredAction: ActionBindCredential,
			ForbiddenClaim: "network unreachable",
		},
		{
			ID:             "mktemp_user_temp_eperm",
			Category:       CategoryFilesystemAccessDenied,
			RequiredAction: ActionEnableSharedUserTemp,
			ForbiddenClaim: "disable GOPROXY because temp is broken",
		},
		{
			ID:             "direct_fallback_unapproved",
			Category:       CategoryNetworkTargetUnapproved,
			RequiredAction: ActionApproveNetworkTarget,
			ForbiddenClaim: "treat Forbidden as the proxy response",
		},
	}
}

type BaselineObservation struct {
	ID             string
	Category       string
	RequiredAction string
	ForbiddenClaim string
}
