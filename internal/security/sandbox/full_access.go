package sandbox

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
)

// Full Access expands workload authority, not authority over the runtime's
// control state. Derive protections from the shared path table and actual
// state layout, including environment grants already selected by the host.
func writeFullAccessProtections(profile *strings.Builder, policy Policy) {
	// Keychain access is also brokered over Mach, so file denies alone do
	// not preserve credential isolation when ordinary OS IPC is allowed.
	// Names come from Apple's securityd launchd MachServices contracts
	// (com.apple.securityd{,.system}.plist) and Security/SecXPCClient.
	for _, service := range []string{"com.apple.SecurityServer", "com.apple.securityd.systemkeychain", "com.apple.securityd", "com.apple.secd"} {
		fmt.Fprintf(profile, "(deny mach-lookup (global-name %s))\n", strconv.Quote(service))
	}
	for _, name := range pathpolicy.ControlPlaneNames() {
		pattern := "^" + regexp.QuoteMeta(policy.WorkspaceRoot) + "/(.*/)?" + caseFoldPathPattern(name) + "(/|$)"
		fmt.Fprintf(profile, "(deny file-write* (regex #%s))\n", seatbeltRegex(pattern))
	}
	for _, location := range pathpolicy.CredentialLocations() {
		if location.Anchor != pathpolicy.AnchorAnywhere {
			continue // Home-anchored protections are emitted by the base profile.
		}
		pattern := "/" + caseFoldPathPattern(strings.Join(location.Segments, "/")) + "(/|$)"
		fmt.Fprintf(profile, "(deny file-read* file-write* (regex #%s))\n", seatbeltRegex(pattern))
	}
	for _, name := range pathpolicy.CredentialFileNames() {
		// Metadata remains available so package managers can probe optional
		// configuration without treating an absent file as a sandbox failure.
		pattern := "/" + caseFoldPathPattern(name) + "$"
		fmt.Fprintf(profile, "(deny file-read-data file-write* (regex #%s))\n", seatbeltRegex(pattern))
	}
	for _, root := range policy.RuntimeStateRoots {
		readGrants := append([]string{policy.PrivateTemp}, policy.HostReadRoots...)
		readGrants = append(readGrants, policy.HostReadFiles...)
		writeGrants := append([]string{policy.PrivateTemp}, policy.HostWriteRoots...)
		writeStateProtection(profile, "file-read*", root, readGrants)
		writeStateProtection(profile, "file-write*", root, writeGrants)
	}
}

// Seatbelt's #"..." regex literal preserves backslashes, unlike a Scheme
// string. Quote delimiters without doubling regex escapes such as \.
func seatbeltRegex(pattern string) string {
	return `"` + strings.ReplaceAll(pattern, `"`, `\"`) + `"`
}

func writeStateProtection(profile *strings.Builder, operation, root string, grants []string) {
	fmt.Fprintf(profile, "(deny %s (require-all (subpath %s)", operation, seatbeltQuote(root))
	for _, grant := range grants {
		if grant == root || !pathContains(root, grant) {
			continue
		}
		// subpath covers the selected directory or exact file, never siblings.
		fmt.Fprintf(profile, " (require-not (subpath %s))", seatbeltQuote(grant))
	}
	profile.WriteString("))\n")
}

func caseFoldPathPattern(path string) string {
	var pattern strings.Builder
	for _, char := range filepath.ToSlash(path) {
		if char >= 'a' && char <= 'z' {
			fmt.Fprintf(&pattern, "[%c%c]", char, char-'a'+'A')
		} else {
			pattern.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	return pattern.String()
}
