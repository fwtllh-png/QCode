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
	ID      CapabilityID `json:"id"`
	Darwin  Support      `json:"darwin"`
	Linux   Support      `json:"linux"`
	Windows Support      `json:"windows"`
}

func PlatformMatrix() []PlatformCapability {
	return []PlatformCapability{
		{
			ID:     CapStrongSandboxReadRoots,
			Darwin: SupportAvailable, Linux: SupportAvailable,
			Windows: SupportUnsupported,
		},
		{
			ID:     CapManagedProxy,
			Darwin: SupportAvailable, Linux: SupportUnsupported,
			Windows: SupportUnsupported,
		},
		{
			ID:     CapResolveUserTemp,
			Darwin: SupportAvailable, Linux: SupportAvailable,
			Windows: SupportAvailable,
		},
		{
			ID:     CapPrivateTmpView,
			Darwin: SupportUnsupported, Linux: SupportNotYet,
			Windows: SupportUnsupported,
		},
		{
			ID:     CapIndependentIdentity,
			Darwin: SupportNotYet, Linux: SupportNotYet,
			Windows: SupportNotYet,
		},
		{
			ID:     CapCertificateFiles,
			Darwin: SupportAvailable, Linux: SupportAvailable,
			Windows: SupportAvailable,
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
		case "linux":
			return capability.Linux, nil
		case "windows":
			return capability.Windows, nil
		default:
			return "", fmt.Errorf("platform %q is not in the capability matrix", goos)
		}
	}
	return "", fmt.Errorf("capability %q is not in the platform matrix", id)
}
