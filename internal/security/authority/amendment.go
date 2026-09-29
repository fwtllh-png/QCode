package authority

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type AdditionalPermissionKind string

const (
	AdditionalPathRead  AdditionalPermissionKind = "path_read"
	AdditionalPathWrite AdditionalPermissionKind = "path_write"
	AdditionalNetwork   AdditionalPermissionKind = "network"
	AdditionalProcess   AdditionalPermissionKind = "process"
)

type AdditionalPermission struct {
	Kind       AdditionalPermissionKind `json:"kind"`
	Resource   string                   `json:"resource"`
	Protocol   string                   `json:"protocol,omitempty"`
	Port       uint16                   `json:"port,omitempty"`
	Capability securitymodel.Capability `json:"capability,omitempty"`
}

type AdditionalPermissionRequest struct {
	BaseProfileDigest string               `json:"base_profile_digest"`
	Permission        AdditionalPermission `json:"permission"`
}

func RequestFromDenial(
	profile EffectivePermissionProfile,
	denial sandbox.Denial,
) (AdditionalPermissionRequest, error) {
	if err := profile.Validate(); err != nil {
		return AdditionalPermissionRequest{}, err
	}
	if !denial.Amendable() {
		return AdditionalPermissionRequest{}, errors.New("sandbox denial is not amendable")
	}
	permission := AdditionalPermission{
		Resource: strings.TrimSpace(denial.Resource),
		Protocol: strings.ToLower(strings.TrimSpace(denial.Protocol)),
		Port:     denial.Port,
	}
	switch denial.Operation {
	case sandbox.DenialRead:
		permission.Kind = AdditionalPathRead
	case sandbox.DenialWrite:
		permission.Kind = AdditionalPathWrite
	case sandbox.DenialNetwork:
		permission.Kind = AdditionalNetwork
	case sandbox.DenialProcess:
		permission.Kind = AdditionalProcess
		permission.Capability = securitymodel.Capability(denial.Resource)
	default:
		return AdditionalPermissionRequest{}, errors.New("unsupported sandbox denial operation")
	}
	request := AdditionalPermissionRequest{
		BaseProfileDigest: profile.Digest,
		Permission:        permission,
	}
	if err := validateAdditionalPermission(profile, permission); err != nil {
		return AdditionalPermissionRequest{}, err
	}
	return request, nil
}

func Amend(
	base EffectivePermissionProfile,
	request AdditionalPermissionRequest,
	revision uint64,
) (EffectivePermissionProfile, error) {
	if err := base.Validate(); err != nil {
		return EffectivePermissionProfile{}, err
	}
	if request.BaseProfileDigest != base.Digest {
		return EffectivePermissionProfile{}, errors.New("additional permission belongs to another profile")
	}
	if revision <= base.Revision {
		return EffectivePermissionProfile{}, errors.New("additional permission revision must advance")
	}
	if err := validateAdditionalPermission(base, request.Permission); err != nil {
		return EffectivePermissionProfile{}, err
	}
	amended := cloneProfile(base)
	permission := request.Permission
	switch permission.Kind {
	case AdditionalPathRead:
		amended.Filesystem.ReadRoots = append(
			amended.Filesystem.ReadRoots,
			filepath.Clean(permission.Resource),
		)
	case AdditionalPathWrite:
		amended.Filesystem.WritePaths = append(
			amended.Filesystem.WritePaths,
			filepath.Clean(permission.Resource),
		)
	case AdditionalNetwork:
		amended.Network.Targets = append(
			amended.Network.Targets,
			networkTarget(permission),
		)
	case AdditionalProcess:
		amended.Process.Allowed = true
	}
	amended.Revision = revision
	amended.Provenance = append(amended.Provenance, AuthoritySource{
		Kind: "amendment", Value: string(permission.Kind),
		Digest: digestJSON(request),
	})
	normalize(&amended)
	digest, err := profileDigest(amended)
	if err != nil {
		return EffectivePermissionProfile{}, err
	}
	amended.Digest = digest
	return amended, amended.Validate()
}

func validateAdditionalPermission(
	profile EffectivePermissionProfile,
	permission AdditionalPermission,
) error {
	switch permission.Kind {
	case AdditionalPathRead:
		return validateAbsolutePath(permission.Resource)
	case AdditionalPathWrite:
		if err := validateAbsolutePath(permission.Resource); err != nil {
			return err
		}
		path := filepath.Clean(permission.Resource)
		if !pathWithin(profile.Filesystem.WorkspaceRoot, path) {
			return errors.New("additional write path is outside the workspace")
		}
		for _, denied := range profile.Filesystem.DeniedWriteRoots {
			if pathWithin(denied, path) {
				return errors.New("additional write path intersects an immutable deny")
			}
		}
		return nil
	case AdditionalNetwork:
		host := strings.TrimSpace(permission.Resource)
		if host == "" || strings.ContainsAny(host, "/\\@") {
			return errors.New("additional network permission requires one host")
		}
		if net.ParseIP(host) == nil && strings.Contains(host, ":") {
			return errors.New("additional network host is invalid")
		}
		switch strings.ToLower(permission.Protocol) {
		case "http", "https", "tcp":
		default:
			return errors.New("additional network protocol is invalid")
		}
		if permission.Port == 0 {
			return errors.New("additional network port is required")
		}
		return nil
	case AdditionalProcess:
		if permission.Capability != securitymodel.CapabilityProcess &&
			permission.Capability != securitymodel.CapabilityExternal {
			return errors.New("additional process capability is invalid")
		}
		return nil
	default:
		return errors.New("additional permission kind is invalid")
	}
}

func validateAbsolutePath(path string) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) ||
		filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("additional permission requires one absolute non-root path")
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func networkTarget(permission AdditionalPermission) string {
	host := permission.Resource
	if strings.Contains(host, ":") {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return strings.ToLower(permission.Protocol) + "://" +
		net.JoinHostPort(host, strconv.Itoa(int(permission.Port)))
}

func cloneProfile(source EffectivePermissionProfile) EffectivePermissionProfile {
	cloned := source
	cloned.Filesystem.ReadRoots = append([]string(nil), source.Filesystem.ReadRoots...)
	cloned.Filesystem.WritePaths = append([]string(nil), source.Filesystem.WritePaths...)
	cloned.Filesystem.DeniedWriteRoots = append(
		[]string(nil),
		source.Filesystem.DeniedWriteRoots...,
	)
	cloned.Network.Targets = append([]string(nil), source.Network.Targets...)
	cloned.Provenance = append([]AuthoritySource(nil), source.Provenance...)
	return cloned
}

func PermissionResource(permission AdditionalPermission) securitymodel.Resource {
	switch permission.Kind {
	case AdditionalPathRead:
		return securitymodel.Resource{Class: securitymodel.ClassPath, Path: permission.Resource, Access: securitymodel.Read}
	case AdditionalPathWrite:
		return securitymodel.Resource{Class: securitymodel.ClassPath, Path: permission.Resource, Access: securitymodel.Write}
	case AdditionalNetwork:
		return securitymodel.Resource{
			Class: securitymodel.ClassNetwork, Access: securitymodel.Write,
			Network: &netpolicy.Target{Scheme: strings.ToLower(permission.Protocol), Host: permission.Resource, Port: permission.Port},
		}
	case AdditionalProcess:
		return securitymodel.Resource{
			Class: securitymodel.ClassProcess, ID: string(permission.Capability),
			Access: securitymodel.Write,
		}
	default:
		panic(fmt.Sprintf("invalid additional permission kind %q", permission.Kind))
	}
}
