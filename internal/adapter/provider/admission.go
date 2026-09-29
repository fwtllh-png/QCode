package provider

// AdmissionReceipt records how one item was bounded before it became
// model-visible. The original payload remains addressable by Handle.
type AdmissionReceipt struct {
	Kind           string `json:"kind"`
	Reason         string `json:"reason"`
	Digest         string `json:"digest"`
	Handle         string `json:"handle,omitempty"`
	OriginalBytes  int    `json:"original_bytes"`
	RetainedBytes  int    `json:"retained_bytes"`
	OriginalTokens uint64 `json:"original_tokens"`
	RetainedTokens uint64 `json:"retained_tokens"`
	TokenLimit     uint64 `json:"token_limit"`
	Truncated      bool   `json:"truncated,omitempty"`
}

func CloneAdmissionReceipt(
	value *AdmissionReceipt,
) *AdmissionReceipt {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
