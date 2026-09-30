// Package contextsnapshot defines the parent-context facts shared by the
// runtime and subagent orchestration. Collection and inheritance policy belong
// to their respective owners, not to this contract.
package contextsnapshot

type SourceRef struct {
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
}

type Block struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role   string  `json:"role"`
	Turn   uint64  `json:"turn"`
	Blocks []Block `json:"blocks"`
}

// MaxRelevantFileExcerptBytes bounds the workspace-fact excerpt attached to
// each delegated relevant file. An excerpt is a delegation hint, not a file
// transfer: the child still reads the full window when the task needs it.
const MaxRelevantFileExcerptBytes = 2 << 10

type RelevantFile struct {
	Path     string   `json:"path"`
	Sources  []string `json:"sources,omitempty"`
	Critical bool     `json:"critical,omitempty"`
	// Excerpt is a bounded prefix of the file's current content. It carries
	// workspace facts only and is redacted and re-bounded by the fork.
	Excerpt string `json:"excerpt,omitempty"`
}

type Evidence struct {
	Summary string `json:"summary"`
	Handle  string `json:"handle"`
}

type Snapshot struct {
	SourceThread    string         `json:"source_thread"`
	SourceTurn      string         `json:"source_turn"`
	AvailableTokens uint64         `json:"available_tokens,omitempty"`
	ParentGoal      string         `json:"parent_goal,omitempty"`
	UserRequest     string         `json:"user_request,omitempty"`
	Messages        []Message      `json:"messages,omitempty"`
	RelevantFiles   []RelevantFile `json:"relevant_files,omitempty"`
	Evidence        []Evidence     `json:"evidence,omitempty"`
	WorkspaceRules  []string       `json:"workspace_rules,omitempty"`
}
