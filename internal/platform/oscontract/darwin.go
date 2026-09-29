// Package oscontract records platform-specific executable requirements with
// their provenance; security compilers consume this data without tool names.
package oscontract

import "path/filepath"

type ExecutableContract struct {
	Executable    string
	Provenance    string
	SeatbeltRules string
}

var darwinExecutables = []ExecutableContract{{
	Executable: "/bin/sh",
	// The Darwin shell regression observes sh-thd-* in the system temp
	// directory despite TMPDIR; lsof resolves /var/tmp to /private/var/tmp.
	// Preserve the deployed compatible sh /private/tmp alias.
	Provenance: "TestSeatbeltShellHereDocumentGrantIsNarrow; platform/process/sandbox_attack_test.go here-document fixture",
	SeatbeltRules: `(allow file-write* (literal "/var/tmp"))
(allow file-write* (regex #"^/var/tmp/sh-thd-[0-9]+$"))
(allow file-read* (regex #"^/var/tmp/sh-thd-[0-9]+$"))
(allow file-write* (literal "/private/var/tmp"))
(allow file-write* (regex #"^/private/var/tmp/sh-thd-[0-9]+$"))
(allow file-read-metadata (subpath "/private/var/tmp"))
(allow file-read* (regex #"^/private/var/tmp/sh-thd-[0-9]+$"))
(allow file-write* (literal "/private/tmp"))
(allow file-write* (regex #"^/private/tmp/sh-thd-[0-9]+$"))
(allow file-read* (regex #"^/private/tmp/sh-thd-[0-9]+$"))
`,
}}

func DarwinExecutable(executable string) (ExecutableContract, bool) {
	for _, contract := range darwinExecutables {
		if filepath.Clean(executable) == contract.Executable {
			return contract, true
		}
	}
	return ExecutableContract{}, false
}
