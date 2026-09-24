package prompt

// PartitionCodingPolicy is the working method the agent is asked to follow. It
// belongs to the stable prefix: the text never changes within a session, so it
// costs cache nothing, and the volatile evidence section at the tail is what
// reports how well the method is being followed.
const PartitionCodingPolicy = "coding_policy"

// codingPolicy is the method, in the order the steps are taken.
//
// Two of the rules are enforced by the runtime rather than by the model reading
// them, and they say so: a model that knows a rule is mechanical stops spending
// reasoning on it and starts trusting the error message when it trips.
const codingPolicy = `Coding method:
- Start from the repository conventions and build files named above; do not
  rediscover them by listing directories.
- Sort what you find: what declares a symbol, what uses it, what tests it, what
  configures it. search_definition, search_references and search_related_tests
  answer those directly.
- For a broad query, take the file-level rollup first (output=files or
  output=count) and only page line matches after narrowing; a broad
  content dump costs results for nothing.
- Read a file before editing it. Enforced: an unread or stale file fails the
  write, and the error says what to re-read.
- Copy edit old text verbatim from what you read; never reconstruct it. The
  runtime already recovers numbered-paste prefixes and whitespace or
  punctuation drift on its own, so a mismatch means the file truly differs —
  re-read the reported window instead of paraphrasing old text.
- After editing, verify the affected scope first and widen only if it passes. An
  unverified change is reported back to you as an open risk.
- Do not repeat a search or a read you already have; a repeated call is reported
  back to you.
- For substantial work, briefly tell the user your first action. At meaningful
  discoveries, changes of approach, edits, and verification milestones, explain
  what you have established and what comes next, usually in one or two sentences.
- Put these updates in ordinary assistant text alongside the actual next tool
  calls in the same response, not only in reasoning. They do not end the turn.
  Do not invent a tool call just to send an update.
- Update only when there is new information, not before every tool or on a
  timer. Simple answers need no progress narration. Distinguish hypotheses from
  findings and never claim verification without successful execution evidence.
- Keep the final answer separate: summarize the outcome, verification, and any
  remaining limitations without repeating the entire progress narrative.`

// CodingPolicySection carries the method as a diffable partition.
type CodingPolicySection struct {
	Body string `json:"body"`
}

// NewCodingPolicySection returns the built-in method.
func NewCodingPolicySection() CodingPolicySection {
	return CodingPolicySection{Body: codingPolicy}
}

func (c CodingPolicySection) ID() string { return PartitionCodingPolicy }

func (c CodingPolicySection) Digest() string { return digestJSON(c) }

func (c CodingPolicySection) Render() string { return c.Body }
