package environment

import "fmt"

type Support string

const (
	SupportAvailable   Support = "available"
	SupportUnsupported Support = "unsupported"
	SupportNotYet      Support = "not_yet"
)

type CapabilityID string

const (
	CapStrongSandboxReadRoots CapabilityID = "strong_sandbox_read_roots"
	CapManagedProxy           CapabilityID = "managed_proxy"
	CapResolveUserTemp        CapabilityID = "resolve_user_temp"
	CapPrivateTmpView         CapabilityID = "private_tmp_view"
	CapIndependentIdentity    CapabilityID = "independent_identity"
	CapCertificateFiles       CapabilityID = "certificate_files"
)

type PlatformCapability struct {
	ID     CapabilityID `json:"id"`
	Darwin Support      `json:"darwin"`
}

func PlatformMatrix() []PlatformCapability {
	return []PlatformCapability{
		{
			ID:     CapStrongSandboxReadRoots,
			Darwin: SupportAvailable,
		},
		{
			ID:     CapManagedProxy,
			Darwin: SupportAvailable,
		},
		{
			ID:     CapResolveUserTemp,
			Darwin: SupportAvailable,
		},
		{
			ID:     CapPrivateTmpView,
			Darwin: SupportUnsupported,
		},
		{
			ID:     CapIndependentIdentity,
			Darwin: SupportNotYet,
		},
		{
			ID:     CapCertificateFiles,
			Darwin: SupportAvailable,
		},
	}
}

func SupportFor(goos string, id CapabilityID) (Support, error) {
	for _, capability := range PlatformMatrix() {
		if capability.ID != id {
			continue
		}
		switch goos {
		case "darwin":
			return capability.Darwin, nil
		default:
			return "", fmt.Errorf("platform %q is not in the capability matrix", goos)
		}
	}
	return "", fmt.Errorf("capability %q is not in the platform matrix", id)
}
