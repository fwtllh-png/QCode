package symbols

// ReferenceSite records a use at UTF-8 byte offsets [StartByte, EndByte).
// Scope is the start byte of its lexical scope within this file version.
// Import and package relationships are candidates, not compiler bindings.
type ReferenceSite struct {
	Name      string `json:"name"`
	Target    string `json:"target"`
	Module    string `json:"module,omitempty"`
	Kind      string `json:"kind"`
	Scope     int    `json:"scope"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Line      int    `json:"line"`
}

const (
	ReferencePackage    = "package_candidate"
	ReferenceImport     = "import_candidate"
	ReferenceUnresolved = "unresolved"
)
