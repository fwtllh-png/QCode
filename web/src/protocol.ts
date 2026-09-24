export type Cursor = number;

export interface Problem {
  version: number;
  code: string;
  message: string;
  retryable: boolean;
  details?: Record<string, unknown>;
}

export interface Envelope<T> {
  version: number;
  request_id?: string;
  result?: T;
  problem?: Problem;
}

export interface WorkspaceIdentity {
  version: number;
  root_id: string;
  editor_uri: string;
  runtime_path: string;
  remote_name?: string;
}

export interface Bootstrap {
  protocol_version: number;
  server_build: string;
  token: string;
  ready: boolean;
  draining: boolean;
  workspace_root?: string;
  workspace?: WorkspaceIdentity;
  setup_required?: boolean;
  workspace_catalog?: WorkspaceCatalog;
  problem?: Problem;
}

// SetupRequest 声明一条 OpenAI-compatible 模型连接的四要素：Base URL、
// Protocol、Model ID、API Key（外加探测或手填的模型元数据）。
export interface SetupRequest {
  model: string;
  api_key?: string;
  base_url: string;
  protocol?: string;
  model_metadata?: SetupModelMetadata;
}

export interface SetupModelMetadata {
  canonical_id: string;
  wire_id: string;
  context_tokens: number;
  max_output_tokens: number;
  capabilities: SetupModelCapabilities;
}

export interface SetupModelCapabilities {
  streaming: boolean;
  reasoning: boolean;
  reasoning_efforts?: string[];
  default_reasoning_effort?: string;
  tool_calls: boolean;
  native_search: boolean;
  incremental_responses?: boolean;
  vision: boolean;
  image_input: boolean;
  prompt_cache: boolean;
  automatic_prompt_cache?: boolean;
  thinking_toggle?: boolean;
}

export interface SetupResult {
  ready: boolean;
}

export interface SetupProbeRequest {
  base_url: string;
  protocol: string;
  model: string;
  api_key?: string;
}

export interface SetupDiscoveredModel {
  id: string;
  name?: string;
  context_tokens?: number;
  max_output_tokens?: number;
}

export interface SetupProbeResult {
  models?: SetupDiscoveredModel[];
  capabilities: SetupModelCapabilities;
  warning?: string;
}

export interface WorkspaceDescriptor {
  id: string;
  root: string;
  label: string;
  ready: boolean;
  removable: boolean;
  session_count: number;
  problem?: string;
  git?: WorkspaceGitState;
}

export interface WorkspaceGitState {
  repository: boolean;
  branch?: string;
  branches?: string[];
  detached?: boolean;
  dirty?: boolean;
}

export interface WorkspaceCatalog {
  version: number;
  workspaces: WorkspaceDescriptor[];
}

export interface WorkspaceAddResult {
  workspace: WorkspaceDescriptor;
}

export interface WorkspaceDirectoryResult {
  path?: string;
  cancelled?: boolean;
}

export interface SessionSummary {
  version: number;
  revision: number;
  session_id: string;
  thread_id: string;
  title: string;
  title_source?: "default" | "temporary" | "auto" | "manual";
  title_revision?: number;
  status: string;
  pinned: boolean;
  archived: boolean;
  isolation: "shared" | "worktree";
  workspace_root: string;
  workspace_label: string;
  provider?: string;
  model?: string;
  mode?: string;
  latest_turn_id?: string;
  latest_turn_withdrawn?: boolean;
  latest_sequence: Cursor;
  pending_approvals: number;
  pending_inputs: number;
  checkpoint_count: number;
  changed_files: number;
  total_tokens: number;
  cost_microunits: number;
  cost_known: boolean;
  created_at: string;
  updated_at: string;
}

export interface SessionList {
  version: number;
  query?: string;
  sessions: SessionSummary[];
}

export interface SessionBinding {
  session_id: string;
  thread_id: string;
  workspace_root: string;
  provider: string;
  model: string;
  isolation: string;
}

export interface SessionLifecycleUpdate {
  session: SessionSummary;
}

export interface SessionDeleteResult {
  version: number;
  session_id: string;
  thread_id: string;
  deleted_at: string;
}

export interface RuntimeEvent {
  version: number;
  id: string;
  kind: string;
  operation_id: string;
  thread_id: string;
  turn_id: string;
  item_id: string;
  sequence: Cursor;
  created_at: string;
  data: Record<string, unknown>;
}

export interface PresentationSnapshot {
  version: number;
  session_id: string;
  thread_id: string;
  session_revision: number;
  through_sequence: Cursor;
  events: RuntimeEvent[];
  history_truncated_before?: Cursor;
}

export interface QueuedTurn {
  queue_id: string;
  thread_id: string;
  source_turn_id: string;
  prompt: string;
  display_prompt?: string;
  intent?: string;
  workspace_identity?: WorkspaceIdentity;
  context?: EditorContextReference[];
  added_sequence: Cursor;
  created_at: string;
  updated_at: string;
}

export interface TurnQueue {
  version: number;
  items: QueuedTurn[];
}

export interface SessionHistoryPage {
  session_id: string;
  events: RuntimeEvent[];
  next_sequence: Cursor;
  more: boolean;
  previous_sequence?: Cursor;
  more_before?: boolean;
}

export interface EventFrame {
  type: "hello" | "event" | "watermark" | "resync" | "desync";
  protocol_version: number;
  session_id?: string;
  sequence: Cursor;
  event?: RuntimeEvent;
  problem?: Problem;
}

export interface OperationReceipt {
  operation_id: string;
  kind: string;
  thread_id: string;
  turn_id: string;
  item_id: string;
  accepted: boolean;
}

export interface SessionProfile {
  version: number;
  revision: number;
  mode: "act";
  planning_policy?: "off" | "adaptive" | "required";
  provider: string;
  model: string;
  reasoning_effort?: string;
  enabled_tool_ids?: string[];
  approval_posture: string;
  execution_target: string;
  max_steps: number;
  prompt_cache_revision?: number;
}

export interface SessionProfileSnapshot {
  profile: SessionProfile;
  capabilities: {
    provider: string;
    model: string;
    mutable_fields: string[];
    model_capabilities: ModelCapabilities;
  };
}

export interface SessionProfileUpdateResult {
  profile: SessionProfile;
  prompt_cache_reset: boolean;
  reset_reason?: string;
}

export interface AgentPresetProfile {
  mode: SessionProfile["mode"];
  planning_policy?: SessionProfile["planning_policy"];
  provider: string;
  model: string;
  reasoning_effort?: string;
  enabled_tool_ids?: string[];
  approval_posture: string;
  execution_target: string;
  max_steps: number;
}

export interface AgentPreset {
  version: number;
  id: string;
  revision: number;
  name: string;
  description?: string;
  scope: "workspace";
  profile: AgentPresetProfile;
  created_at: string;
  updated_at: string;
}

export interface AgentPresetList {
  version: number;
  revision: number;
  presets: AgentPreset[];
}

export interface AgentPresetMutationResult {
  version: number;
  revision: number;
  preset?: AgentPreset;
  deleted_id?: string;
  duplicate?: boolean;
}

export interface AgentPresetApplyResult {
  version: number;
  preset_id: string;
  profile_update: SessionProfileUpdateResult;
  restart_required: boolean;
  restart_reason?: string;
}

export interface ProviderCatalogEntry {
  id: string;
  display_name: string;
  selected: boolean;
  availability: "available" | "unavailable";
  reason?: string;
}

export interface ProviderCatalog {
  version: number;
  providers: ProviderCatalogEntry[];
}

export interface ModelCapabilities {
  display_name: string;
  context_window: number;
  max_output_tokens: number;
  streaming: boolean;
  reasoning: boolean;
  tool_calls: boolean;
  parallel_tool_calls: "supported" | "unsupported" | "unknown";
  native_search: boolean;
  incremental_responses: boolean;
  vision: boolean;
  image_input: boolean;
  prompt_cache: boolean;
  automatic_prompt_cache: boolean;
  thinking_toggle: boolean;
  reasoning_efforts?: string[];
  default_reasoning_effort?: string;
  metadata_provenance: ModelMetadataProvenance;
  credential_status: "configured" | "missing" | "invalid" | "unknown";
  availability: "available" | "unavailable";
  unavailable_reason?: string;
  selection_mode: "hot" | "restart_required" | "fixed";
}

export interface ModelMetadataProvenance {
  canonical_id: string;
  wire_id: string;
  limits: string;
  capabilities: string;
  pricing: string;
}

export interface ModelCatalogEntry {
  provider: string;
  id: string;
  source?: "catalog" | "connection_baseline" | "registered";
  selected: boolean;
  capabilities: ModelCapabilities;
}

export interface ModelCatalog {
  version: number;
  models: ModelCatalogEntry[];
}

export interface ModelTestResult {
  provider: string;
  model: string;
  status: "available" | "not_listed";
  detail: string;
  tested_at: string;
}

export interface ModelMutationRequest {
  model: string;
  model_metadata: SetupModelMetadata;
}

export interface WorkspaceConnection {
  provider: string;
  endpoint: string;
  protocol: string;
  model_metadata?: SetupModelMetadata;
}

export interface ConnectionEntry {
  id: string;
  provider: string;
  display_name?: string;
  base_url?: string;
  protocol?: string;
  model: string;
  models?: string[];
  default?: boolean;
  credential_present: boolean;
}

export interface ConnectionListResult {
  version: number;
  default_connection: string;
  connections: ConnectionEntry[];
}

export interface ToolCatalogEntry {
  id: string;
  name: string;
  description: string;
  source_kind: string;
  source_label: string;
  capability: string;
  access_mode: string;
  risk_level: string;
  sandbox_requirement: string;
  policy_state: string;
  policy_reason: string;
  constitution_state: string;
  constitution_reason: string;
  availability: string;
  unavailable_reason?: string;
  state: string;
  revision: number;
  enabled: boolean;
  guarded: boolean;
}

export interface ToolCatalog {
  version: number;
  catalog_id: string;
  generation: number;
  digest: string;
  tools: ToolCatalogEntry[];
}

export interface SessionCheckpoint {
  version: number;
  id: string;
  session_id: string;
  thread_id: string;
  turn_id: string;
  cursor: Cursor;
  status: "completed" | "interrupted";
  summary: string;
  profile_revision: number;
  changed_files: number;
  external_side_effects: boolean;
  can_restore: boolean;
  can_fork: boolean;
  created_at: string;
}

export interface CheckpointList {
  version: number;
  session_id: string;
  checkpoints: SessionCheckpoint[];
}

export interface CheckpointForkResult {
  version: number;
  checkpoint: SessionCheckpoint;
  session_id: string;
  thread_id: string;
  parent_thread_id: string;
  exact_context: boolean;
  workspace_claims_valid: boolean;
}

export interface SessionPlanArtifact {
  version: number;
  id: string;
  session_id: string;
  thread_id: string;
  turn_id: string;
  cursor: Cursor;
  status: "ready";
  purpose?: "execution" | "deliverable";
  body: string;
  document?: PlanDocument;
  profile_revision: number;
  execution_profile_digest?: string;
  can_implement: boolean;
  can_autopilot: boolean;
  created_at: string;
}

export interface PlanStep {
  id: string;
  title: string;
  status: "pending" | "in_progress" | "done";
  dependencies?: string[];
  expected_evidence?: string;
  affected_files?: string[];
}

export interface PlanDocument {
  version: 1;
  purpose?: "execution" | "deliverable";
  revision?: number;
  supersedes_id?: string;
  title?: string;
  objective?: string;
  context_summary?: string;
  steps: PlanStep[];
  sources_used?: string[];
  critical_files?: string[];
  constraints?: string[];
  recommended_approach?: string;
  verification_plan?: string;
  risks_and_unknowns?: string;
  handoff_packet?: string;
  file_baseline?: Array<{
    path: string;
    digest?: string;
    missing?: boolean;
  }>;
}

export interface SessionPlanSnapshot {
  version: number;
  artifact?: SessionPlanArtifact;
}

export interface AgentSummary {
  id: string;
  role: string;
  status: string;
  last_message?: string;
}

export interface UsageRollup {
  activity?: {
    turns: number;
    completed: number;
    failed: number;
    canceled: number;
    tool_calls: number;
  };
  turns: number;
  calls: number;
  total_tokens: number;
  cost_microunits: number;
  cost_known: boolean;
  priced_calls?: number;
  unpriced_calls?: number;
}

export interface UsageAggregate {
  session_id: string;
  thread_id: string;
  turn_id: string;
  provider: string;
  model: string;
  model_metadata_provenance?: ModelMetadataProvenance;
  input_tokens: number;
  output_tokens: number;
  reasoning_tokens: number;
  cached_tokens: number;
  cost_microunits: number;
  priced_calls: number;
  unpriced_calls: number;
  calls: number;
  first_at: string;
  last_at: string;
}

export interface AgentList {
  agents: AgentSummary[];
}

export interface UsageQueryResult {
  usage: UsageAggregate[];
  rollup: UsageRollup;
}

export type TraceSpanKind =
  | "turn"
  | "model"
  | "tool"
  | "approval"
  | "verification";

export interface TraceSpan {
  id: number;
  parent_id?: number;
  kind: TraceSpanKind;
  status: "open" | "ok" | "error" | "canceled";
  started_at: string;
  ended_at?: string;
  duration_ms?: number;
  sample?: number;
  call_id?: string;
}

export interface TraceTurn {
  turn_id: string;
  complete?: boolean;
  started_at?: string;
  ended_at?: string;
  status: "unavailable" | "open" | "ok" | "error" | "canceled";
  spans: TraceSpan[];
}

export interface TraceSnapshot {
  version: number;
  session_id: string;
  through_sequence: Cursor;
  turns: TraceTurn[];
}

export interface ExtensionProjection {
  kind: "skill";
  name: string;
  version?: string;
  source?: string;
  publisher?: string;
  trust?: string;
  digest?: string;
  generation?: number;
  enabled: boolean;
  health: string;
  permissions?: string[];
  capabilities?: Array<{
    id: string;
    kind: string;
    enabled: boolean;
    source_digest?: string;
    permission_digest?: string;
    authority_token?: string;
  }>;
  last_action?: string;
  changed_at?: string;
}

export type ExtensionControlAction =
  | "detail"
  | "health"
  | "permissions"
  | "receipts"
  | "enable"
  | "disable"
  | "revoke"
  | "lint"
  | "lock"
  | "verify";

export interface ExtensionControlResult {
  operation_id?: string;
  duplicate?: boolean;
  revision: number;
  extensions?: ExtensionProjection[];
  detail?: unknown;
  receipt?: {
    operation_id: string;
    action: string;
    kind: string;
    name?: string;
    status: string;
    digest: string;
    revision: number;
    occurred_at: string;
  };
  receipts?: Array<{
    operation_id: string;
    action: string;
    kind: string;
    name?: string;
    status: string;
    digest: string;
    revision: number;
    occurred_at: string;
  }>;
  diagnostics?: unknown;
}

export interface EditPlanFile {
  path: string;
  kind: string;
}

export interface EditPlan {
  id: string;
  diff: string;
  files: EditPlanFile[];
}

export interface SessionMergeResult {
  session_id: string;
  thread_id: string;
  action: "preview" | "apply";
  plan: EditPlan;
}

export interface WorkspaceEntry {
  path: string;
  kind: "file" | "directory";
  size?: number;
}

export interface WorkspaceBrowseResult {
  path: string;
  entries: WorkspaceEntry[];
  more: boolean;
}

export interface WorkspaceSearchMatch {
  path: string;
  line: number;
  column: number;
  preview: string;
}

export interface WorkspaceSearchResult {
  query: string;
  matches: WorkspaceSearchMatch[];
  more: boolean;
}

export interface WorkspaceResource {
  path: string;
  uri: string;
  document_version: number;
  content: string;
  digest: string;
  bytes: number;
  content_handle: string;
}

export interface WorkspaceImage {
  path: string;
  uri: string;
  document_version: number;
  digest: string;
  bytes: number;
  media_type: "image/png" | "image/jpeg" | "image/gif" | "image/webp";
  label: string;
  content_handle: string;
}

export interface EditorPosition {
  line: number;
  character: number;
}

export interface EditorRange {
  start: EditorPosition;
  end: EditorPosition;
}

export interface EditorSymbol {
  name: string;
  kind: string;
  selection_range: EditorRange;
}

export interface EditorDiagnostic {
  range: EditorRange;
  severity: "error" | "warning" | "information" | "hint";
  code?: string;
  message: string;
  source?: string;
}

export interface EditorContextReference {
  kind: "file" | "selection" | "symbol" | "diagnostics" | "image" | "attachment" | "terminal" | "git_diff";
  source: "composer" | "selection_command" | "code_action" | "native_picker";
  uri?: string;
  path?: string;
  document_version?: number;
  digest: string;
  range?: EditorRange;
  symbol?: EditorSymbol;
  diagnostics?: EditorDiagnostic[];
  omitted_diagnostics?: number;
  label?: string;
  media_type?: string;
  content?: string;
  explicit: true;
}

export interface WorkspaceSymbol {
  path: string;
  name: string;
  kind: string;
  container?: string;
  line: number;
  uri: string;
  document_version: number;
  digest: string;
  range: EditorRange;
  selection_range: EditorRange;
}

export interface WorkspaceSymbolList {
  query: string;
  status: string;
  detail?: string;
  symbols: WorkspaceSymbol[];
}

export interface WorkspaceDiagnosticContext {
  call_id: string;
  tool: string;
  status: string;
  message?: string;
  context: EditorContextReference;
}

export interface WorkspaceDiagnostics {
  session_id: string;
  thread_id: string;
  diagnostics: WorkspaceDiagnosticContext[];
}

export interface WorkspaceDiff {
  session_id: string;
  thread_id: string;
  diff: string;
  digest: string;
}

export interface GitChangeStat {
  added: number;
  removed: number;
  binary?: boolean;
}

export interface GitChange {
  path: string;
  index: string;
  worktree: string;
  untracked?: boolean;
  conflict?: boolean;
  staged?: GitChangeStat;
  unstaged?: GitChangeStat;
}

export interface GitOverview extends WorkspaceGitState {
  revision: string;
  head: string;
  root: boolean;
  files: GitChange[];
  remotes: string[];
}

export interface GitPatch {
  path: string;
  staged: boolean;
  diff: string;
}

export interface GitActionRequest {
  action: "commit" | "commit_push" | "push" | "create_branch";
  branch: string;
  revision: string;
  message?: string;
  paths?: string[];
  include_unstaged?: boolean;
  remote?: string;
  new_branch?: string;
}

export interface GitActionResult {
  action: string;
  commit_hash?: string;
  completed: string[];
  problem?: Problem;
}

export interface CredentialStatus {
  reference: {
    kind: string;
    name: string;
  };
  configured: boolean;
  validation: "not_validated" | "valid" | "invalid";
  validation_detail?: string;
  validated_at?: string;
  restart_required?: boolean;
}
