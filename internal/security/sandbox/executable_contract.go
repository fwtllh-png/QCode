package sandbox

import "path/filepath"

// executableContract records platform-specific requirements and their
// provenance separately from the sandbox profile compiler.
type executableContract struct {
	executable    string
	provenance    string
	seatbeltRules string
}

var darwinExecutableContracts = []executableContract{{
	executable: "/bin/sh",
	// The Darwin shell regression observes sh-thd-* in the system temp
	// directory despite TMPDIR; lsof resolves /var/tmp to /private/var/tmp.
	// Preserve the deployed compatible sh /private/tmp alias.
	provenance: "TestSeatbeltShellHereDocumentGrantIsNarrow; platform/process/process_capability_test.go here-document fixture",
	seatbeltRules: `(allow file-write* (literal "/var/tmp"))
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

func darwinExecutableContract(executable string) (executableContract, bool) {
	for _, contract := range darwinExecutableContracts {
		if filepath.Clean(executable) == contract.executable {
			return contract, true
		}
	}
	return executableContract{}, false
}
