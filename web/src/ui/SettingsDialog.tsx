import {
  Activity,
  Bell,
  Bot,
  Boxes,
  Check,
  ChevronDown,
  Copy,
  Database,
  KeyRound,
  Monitor,
  Moon,
  Plus,
  RefreshCw,
  Save,
  Search,
  Settings2,
  ShieldCheck,
  Sun,
  Trash2,
  Wrench,
  X
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode
} from "react";
import {useModalFocus} from "./primitives/useModalFocus";
import {Presence} from "./primitives/Presence";
import {Switch} from "./primitives/Switch";
import type {
  AgentPreset,
  AgentPresetApplyResult,
  AgentPresetProfile,
  CredentialStatus,
  ModelCatalogEntry,
  ModelTestResult,
  SessionProfile,
  SessionProfileUpdateResult,
  SetupModelMetadata,
  ConnectionEntry,
  ConnectionListResult,
  SetupRequest,
  WorkspaceConnection
} from "../protocol";
import type {RuntimeClient, RuntimeSnapshot} from "../runtime/client";
import type {BrowserNotificationSettings} from "./backgroundActivity";
import {
  initialBrowserNotificationSettings,
  setBrowserNotificationsEnabled
} from "./browserNotifications";
import {
  emptyModelMetadataDraft,
  type ModelMetadataDraft,
  ModelMetadataFields,
  modelMetadataDraft,
  modelMetadataFromProbe,
  modelMetadataProblem,
  setupModelMetadata
} from "./ModelMetadataFields";
import "./SettingsDialog.css";

export type ThemeMode = "light" | "dark" | "system";

export type SettingsSection =
  | "general"
  | "models"
  | "tools"
  | "extensions"
  | "agent";

interface ProfileDraft {
  mode: SessionProfile["mode"];
  provider: string;
  model: string;
  reasoningEffort: string;
  enabledToolIDs: string[];
  approvalPosture: string;
  executionTarget: string;
  maxSteps: number;
}

interface ApplyNotice {
  tone: "success" | "warning";
  text: string;
}

interface Props {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  newIsolation: "shared" | "worktree";
  theme: ThemeMode;
  initialSection?: SettingsSection;
  onIsolationChange: (value: "shared" | "worktree") => void;
  onThemeChange: (value: ThemeMode) => void;
  onClose: () => void;
  onError: (error: unknown) => void;
}

function apiKeyError(value: string): string {
  if (!value) return "";
  if (value.trim() !== value || !/^[\x21-\x7e]+$/.test(value)) {
    return "Enter the API key only, without spaces or quotes.";
  }
  if (/^[A-Z][A-Z0-9_]*=[^=]/.test(value) ||
      (/^([\"'`]).*\1$/.test(value))) {
    return "Enter the API key value, not an environment assignment.";
  }
  return "";
}

const sections: readonly {
  id: SettingsSection;
  label: string;
  icon: typeof Settings2;
}[] = [
  {id: "general", label: "General", icon: Settings2},
  {id: "models", label: "Models", icon: Database},
  {id: "tools", label: "Tools", icon: Wrench},
  {id: "extensions", label: "Extensions", icon: Boxes},
  {id: "agent", label: "Agent preset", icon: Bot}
];

export function SettingsDialog({
  snapshot,
  client,
  newIsolation,
  theme,
  initialSection = "general",
  onIsolationChange,
  onThemeChange,
  onClose,
  onError
}: Props) {
  const [active, setActive] = useState<SettingsSection>(initialSection);
  const [credential, setCredential] = useState<CredentialStatus>();
  const [toolQuery, setToolQuery] = useState("");
  const [error, setError] = useState("");
  const [profileDraft, setProfileDraft] = useState(
    () => settingsProfileDraft(snapshot)
  );
  const [profileBaseline, setProfileBaseline] = useState(
    () => settingsProfileDraft(snapshot)
  );
  const [applying, setApplying] = useState(false);
  const [applyNotice, setApplyNotice] = useState<ApplyNotice>();
  const [notificationSettings, setNotificationSettings] = useState(
    initialBrowserNotificationSettings
  );
  const [notificationPending, setNotificationPending] = useState(false);
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLElement>(null);
  useModalFocus(dialogRef, true, onClose);
  const profileDraftRef = useRef(profileDraft);
  const profileBaselineRef = useRef(profileBaseline);
  const profileOwnerRef = useRef(snapshot.selectedSessionID);
  profileDraftRef.current = profileDraft;
  profileBaselineRef.current = profileBaseline;
  const dirty = !equalProfileDraft(profileDraft, profileBaseline);
  const profileDraftProblem = modelIDProblem(profileDraft.model);
  const profileApplyBlocked = Boolean(snapshot.conversation.activeTurnID);
  const reportError = useCallback((value: unknown) => {
    setError(value instanceof Error ? value.message : String(value));
    onError(value);
  }, [onError]);
  const changeNotifications = useCallback(async (enabled: boolean) => {
    if (notificationPending) return;
    setNotificationPending(true);
    try {
      setNotificationSettings(await setBrowserNotificationsEnabled(enabled));
    } catch (notificationError) {
      reportError(notificationError);
    } finally {
      setNotificationPending(false);
    }
  }, [notificationPending, reportError]);
  const requestClose = useCallback(() => {
    onClose();
  }, [onClose]);
  useEffect(() => {
    const next = settingsProfileDraft(snapshot);
    const sessionChanged = profileOwnerRef.current !== snapshot.selectedSessionID;
    if (
      sessionChanged ||
      equalProfileDraft(profileDraftRef.current, profileBaselineRef.current)
    ) {
      profileOwnerRef.current = snapshot.selectedSessionID;
      setProfileDraft(next);
      setProfileBaseline(next);
      setApplyNotice(undefined);
    }
  }, [
    snapshot.profile?.profile.revision,
    snapshot.selectedSessionID,
    snapshot.tools
  ]);

  useEffect(() => {
    // credential/* 属于 Runtime RPC；模型未配置（setup 阶段）时不可用。
    if (snapshot.phase === "setup") return;
    void client.credentialStatus().then(setCredential, reportError);
  }, [client, reportError, snapshot.phase]);

  const changeProfileDraft = (patch: Partial<ProfileDraft>) => {
    setProfileDraft((current) => current ? {...current, ...patch} : current);
    setApplyNotice(undefined);
  };

  const applyProfile = async () => {
    if (!profileDraft || !profileBaseline || !dirty || applying) return;
    const patch = changedProfileFields(profileBaseline, profileDraft);
    setApplying(true);
    setError("");
    try {
      const result = await client.updateProfile(patch);
      const next = settingsProfileDraftFromProfile(
        result.profile,
        snapshot.tools
      );
      setProfileDraft(next);
      setProfileBaseline(next);
      setApplyNotice(profileApplyNotice(result, profileBaseline, next));
    } catch (applyError) {
      reportError(applyError);
    } finally {
      setApplying(false);
    }
  };

  const acceptAppliedPreset = (result: AgentPresetApplyResult) => {
    const next = settingsProfileDraftFromProfile(
      result.profile_update.profile,
      snapshot.tools
    );
    setProfileDraft(next);
    setProfileBaseline(next);
    setApplyNotice(result.restart_required
      ? {
          tone: "warning",
          text: result.restart_reason || "Applied; Runtime restart required"
        }
      : profileApplyNotice(result.profile_update));
  };

  return (
    <div className="settingsOverlay" data-motion-backdrop role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) requestClose();
    }}>
      <section
        ref={dialogRef}
        className="settingsDialog"
        data-motion-surface
        role="dialog"
        aria-modal="true"
        aria-labelledby="settings-title"
      >
        <nav className="settingsNav" aria-label="Settings sections">
          <h2 id="settings-title">Settings</h2>
          <div>
            {sections
              .filter((section) => snapshot.phase !== "setup" ||
                section.id === "general" || section.id === "models")
              .map((section) => {
              const Icon = section.icon;
              return (
                <button
                  type="button"
                  key={section.id}
                  aria-label={section.label}
                  aria-current={active === section.id ? "page" : undefined}
                  title={section.label}
                  onClick={() => setActive(section.id)}
                >
                  <Icon size={16} />
                  <span>{section.label}</span>
                </button>
              );
            })}
          </div>
        </nav>
        <div className="settingsMain">
          <header className="settingsHeader">
            <button
              ref={closeRef}
              type="button"
              className="settingsClose"
              aria-label="Close settings"
              onClick={requestClose}
            >
              <X size={18} />
            </button>
          </header>
          <div className="settingsContent">
            {error && <div className="settingsError" role="alert">{error}</div>}
            {active === "general" && (
              <GeneralSettings
                snapshot={snapshot}
                isolation={newIsolation}
                theme={theme}
                notificationSettings={notificationSettings}
                notificationPending={notificationPending}
                onIsolationChange={onIsolationChange}
                onThemeChange={onThemeChange}
                onNotificationsChange={(enabled) => void changeNotifications(enabled)}
              />
            )}
            {active === "models" && (
              <ModelSettings
                snapshot={snapshot}
                draft={profileDraft}
                onDraftChange={changeProfileDraft}
                client={client}
                credential={credential}
                onCredential={setCredential}
                onConfigured={requestClose}
                onError={reportError}
              />
            )}
            {active === "tools" && (
              <ToolSettings
                snapshot={snapshot}
                draft={profileDraft}
                onDraftChange={changeProfileDraft}
                query={toolQuery}
                onQueryChange={setToolQuery}
              />
            )}
            {active === "extensions" && (
              <ExtensionSettings
                snapshot={snapshot}
                client={client}
                onError={reportError}
              />
            )}
            {active === "agent" && (
              <AgentSettings
                snapshot={snapshot}
                client={client}
                draft={profileDraft}
                onDraftChange={changeProfileDraft}
                onAppliedPreset={acceptAppliedPreset}
                applyBlocked={profileApplyBlocked}
                onError={reportError}
              />
            )}
          </div>
          {true && (
          <footer className="settingsApplyBar" data-dirty={dirty || undefined}>
            <span aria-live="polite">
              {dirty
                ? profileDraftProblem
                  ? profileDraftProblem
                  : profileApplyBlocked
                  ? "Unsaved changes · finish the active Turn to apply"
                  : "Unsaved changes"
                : applyNotice?.text || "Settings are up to date"}
            </span>
            {applyNotice && !dirty && (
              <small data-tone={applyNotice.tone}>
                {applyNotice.tone === "success"
                  ? <Check size={13} />
                  : <RefreshCw size={13} />}
                {applyNotice.tone === "success" ? "Applied" : "Restart required"}
              </small>
            )}
            <div>
              <button
                type="button"
                disabled={!dirty || applying}
                onClick={() => {
                  setProfileDraft(profileBaseline);
                  setApplyNotice(undefined);
                }}
              >
                Discard
              </button>
              <button
                type="button"
                className="settingsApply"
                disabled={
                  !dirty || applying || profileApplyBlocked ||
                  Boolean(profileDraftProblem)
                }
                title={profileApplyBlocked
                  ? "Finish the active Turn before applying settings"
                  : "Apply changes to this Session"}
                onClick={() => void applyProfile()}
              >
                <Save size={14} />
                {applying ? "Applying..." : "Apply changes"}
              </button>
            </div>
          </footer>
          )}
        </div>
      </section>
    </div>
  );
}

function GeneralSettings({
  snapshot,
  isolation,
  theme,
  notificationSettings,
  notificationPending,
  onIsolationChange,
  onThemeChange,
  onNotificationsChange
}: {
  snapshot: RuntimeSnapshot;
  isolation: "shared" | "worktree";
  theme: ThemeMode;
  notificationSettings: BrowserNotificationSettings;
  notificationPending: boolean;
  onIsolationChange: (value: "shared" | "worktree") => void;
  onThemeChange: (value: ThemeMode) => void;
  onNotificationsChange: (enabled: boolean) => void;
}) {
  return (
    <SettingsSectionView
      title="General"
      description="Browser preferences and defaults for newly created sessions."
    >
      <SettingRow
        title="New session isolation"
        description="Choose whether new sessions share the workspace or use an isolated worktree."
      >
        <SelectControl
          label="New session isolation"
          value={isolation}
          values={["shared", "worktree"]}
          onChange={(value) => onIsolationChange(value as "shared" | "worktree")}
        />
      </SettingRow>
      <div className="settingsBlock">
        <div className="settingsBlockTitle">Appearance</div>
        <div className="themeChoices" role="radiogroup" aria-label="Appearance">
          <ThemeChoice
            label="Light"
            icon={<Sun size={20} />}
            selected={theme === "light"}
            onClick={() => onThemeChange("light")}
          />
          <ThemeChoice
            label="Dark"
            icon={<Moon size={20} />}
            selected={theme === "dark"}
            onClick={() => onThemeChange("dark")}
          />
          <ThemeChoice
            label="System"
            icon={<Monitor size={20} />}
            selected={theme === "system"}
            onClick={() => onThemeChange("system")}
          />
        </div>
      </div>
      <SettingRow
        title="Desktop notifications"
        description="Only background Session status is shown. Prompts and tool output are excluded."
      >
        <label className="settingsPreferenceControl">
          <Bell size={15} aria-hidden="true" />
          <Switch
            label="Desktop notifications"
            checked={notificationSettings.enabled}
            disabled={
              notificationPending ||
              notificationSettings.permission === "unsupported" ||
              notificationSettings.permission === "denied"
            }
            onChange={(next) => onNotificationsChange(next)}
          />
          <span>
            {notificationPending
              ? "Requesting"
              : notificationSettings.enabled
                ? "On"
                : notificationSettings.permission === "denied"
                  ? "Blocked"
                  : notificationSettings.permission === "unsupported"
                    ? "Unavailable"
                    : "Off"}
          </span>
        </label>
      </SettingRow>
      <SettingRow
        title="Runtime connection"
        description={snapshot.workspaceRoot || "Workspace unavailable"}
      >
        <span className="settingsStatus" data-ready={snapshot.socketConnected || undefined}>
          <span />
          {snapshot.socketConnected ? "Connected" : snapshot.phase}
        </span>
      </SettingRow>
    </SettingsSectionView>
  );
}

function ModelSettings({
  snapshot,
  draft,
  onDraftChange,
  client,
  credential,
  onCredential,
  onConfigured,
  onError
}: {
  snapshot: RuntimeSnapshot;
  draft: ProfileDraft;
  onDraftChange: (patch: Partial<ProfileDraft>) => void;
  client: RuntimeClient;
  credential?: CredentialStatus;
  onCredential: (status: CredentialStatus) => void;
  onConfigured: () => void;
  onError: (error: unknown) => void;
}) {
  const catalogModel = snapshot.models.find(
    (model) => model.provider === draft.provider && model.id === draft.model
  );
  // 会话模型跨全部已配置连接选择（不同 Base URL 的连接均可用）。
  const availableModels = snapshot.models.filter(
    (model) => model.capabilities.availability === "available"
  );
  const selectedModelValue = `${draft.provider}\u0000${draft.model}`;
  const profileCapabilities = snapshot.profile?.capabilities.model_capabilities;
  const selectedModel = catalogModel ?? (
    profileCapabilities && draft.model.trim()
      ? {
          provider: draft.provider,
          id: draft.model,
          source: "connection_baseline" as const,
          selected: true,
          capabilities: {
            ...profileCapabilities,
            display_name: draft.model,
            selection_mode: "hot" as const
          }
        }
      : undefined
  );
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<ModelTestResult>();
  const advertisedReasoningValues =
    selectedModel?.capabilities.reasoning_efforts ?? [];
  const reasoningValues = selectedModel?.capabilities.default_reasoning_effort
    ? advertisedReasoningValues
    : ["", ...advertisedReasoningValues];
  const changeModel = (value: string) => {
    setTestResult(undefined);
    const separator = value.indexOf("\u0000");
    const provider = value.slice(0, separator);
    const modelID = value.slice(separator + 1);
    const model = availableModels.find(
      (entry) => entry.provider === provider && entry.id === modelID
    );
    onDraftChange({
      provider,
      model: modelID,
      reasoningEffort: model
        ? model.capabilities.default_reasoning_effort ?? ""
        : draft.reasoningEffort
    });
  };
  const testModel = async () => {
    if (testing || modelIDProblem(draft.model)) return;
    setTesting(true);
    setTestResult(undefined);
    try {
      setTestResult(await client.testModel(draft.model.trim()));
    } catch (error) {
      onError(error);
    } finally {
      setTesting(false);
    }
  };
  return (
    <SettingsSectionView
      title="Models"
      description="Manage connections and choose the model QCode uses for this session."
    >
      <ConnectionsBlock
        snapshot={snapshot}
        client={client}
        credential={credential}
        onCredential={onCredential}
        onConfigured={onConfigured}
        onError={onError}
        onModelAdded={(model, metadata) => onDraftChange({
          model,
          reasoningEffort: metadata.capabilities.default_reasoning_effort ?? ""
        })}
      />
      <div className="settingsBlock">
        <div className="settingsBlockTitle">Session model</div>
        <p>Select a configured model, or add another model.</p>
          <div className="settingsButtonRow modelSelectionPrimary">
            <SelectControl
              label="Settings model"
              value={selectedModelValue}
              values={availableModels.map(
                (model) => `${model.provider}\u0000${model.id}`
              )}
              disabled={!mutable(snapshot, "model")}
              format={(value) => {
                const separator = value.indexOf("\u0000");
                const provider = value.slice(0, separator);
                const modelID = value.slice(separator + 1);
                return provider === draft.provider
                  ? modelID
                  : `${provider} · ${modelID}`;
              }}
              onChange={changeModel}
            />
          </div>
          <div className="settingsButtonRow">
            <button
              type="button"
              disabled={testing || Boolean(modelIDProblem(draft.model))}
              onClick={() => void testModel()}
            >
              {testing
                ? <><RefreshCw className="spin" size={13} /> Testing...</>
                : <><Activity size={13} /> Test model</>}
            </button>
            {testResult && (
              <span
                className="settingsInlineResult"
                data-tone={
                  testResult.status === "available" ? undefined : "warning"
                }
                role="status"
              >
                {testResult.status === "available" && <Check size={13} />}
                {testResult.detail}
              </span>
            )}
          </div>
      </div>
      {selectedModel?.capabilities.reasoning && (
        <SettingRow
          title="Reasoning effort"
          description="Choose how much time the model spends thinking before it answers."
        >
          <SelectControl
            label="Settings reasoning effort"
            value={
              reasoningValues.includes(draft.reasoningEffort)
                ? draft.reasoningEffort
                : selectedModel.capabilities.default_reasoning_effort ??
                  reasoningValues[0] ??
                  ""
            }
            values={reasoningValues}
            disabled={!mutable(snapshot, "reasoning_effort")}
            onChange={(reasoningEffort) => onDraftChange({reasoningEffort})}
          />
        </SettingRow>
      )}
      {selectedModel && (
        <ModelCapabilityPanel model={selectedModel} />
      )}
    </SettingsSectionView>
  );
}

// ConnectionsBlock：连接集合管理块（连接列表 + 添加/更新表单 + 凭证面板），
// 嵌入 ModelSettings——连接与模型同属模型管理。
function ConnectionsBlock({
  snapshot,
  client,
  credential,
  onCredential,
  onConfigured,
  onError,
  onModelAdded
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  credential?: CredentialStatus;
  onCredential: (status: CredentialStatus) => void;
  onConfigured: () => void;
  onError: (error: unknown) => void;
  onModelAdded: (model: string, metadata: SetupModelMetadata) => void;
}) {
  const unconfigured = snapshot.phase === "setup";
  const [connections, setConnections] = useState<ConnectionListResult>();
  const [connectionBusy, setConnectionBusy] = useState("");
  // 向导仅在首次配置（setup 阶段）自动展开；其余入口由用户显式打开。
  const [wizardOpen, setWizardOpen] = useState(unconfigured);
  const [wizardTarget, setWizardTarget] = useState<ConnectionEntry>();

  useEffect(() => {
    void client.listConnections().then(setConnections, () => undefined);
  }, [client]);

  const refresh = async () => {
    try {
      setConnections(await client.listConnections());
    } catch {
      // 列表刷新失败不阻断主流程。
    }
  };

  const runConnectionAction = async (
    label: string,
    action: () => Promise<ConnectionListResult>
  ) => {
    if (connectionBusy) return;
    setConnectionBusy(label);
    try {
      setConnections(await action());
    } catch (error) {
      onError(error);
    } finally {
      setConnectionBusy("");
    }
  };

  return (
    <div className="settingsBlock settingsConnectionsBlock">
      <div className="settingsBlockTitle">Connections</div>
      <p className="settingsBlockDescription">
        Configure one or more model providers; sessions switch models across
        connections from the Composer.
      </p>
      {connections?.connections.map((entry) => (
        <SettingRow
          key={entry.id}
          title={entry.display_name || entry.provider}
          description={`模型 ${entry.model}${entry.base_url ? ` · ${entry.base_url}` : ""}${entry.default ? " · 默认" : ""}`}
        >
          <span className="settingsStatus" data-ready={entry.default || undefined}>
            <span />
            {entry.default ? "Default" : entry.credential_present ? "Ready" : "Key missing"}
          </span>
          <button
            type="button"
            className="settingsHeaderAction"
            onClick={() => {
              setWizardTarget(entry);
              setWizardOpen(true);
            }}
          >
            Edit
          </button>
          {!entry.default && (
            <>
              <button
                type="button"
                className="settingsHeaderAction"
                disabled={Boolean(connectionBusy)}
                onClick={() => void runConnectionAction(
                  `default:${entry.id}`,
                  () => client.setDefaultConnection(entry.id).then((result) => {
                    void refresh();
                    return result;
                  })
                )}
              >
                {connectionBusy === `default:${entry.id}` ? "Switching..." : "Set default"}
              </button>
              <button
                type="button"
                className="settingsHeaderAction"
                disabled={Boolean(connectionBusy)}
                onClick={() => void runConnectionAction(
                  `remove:${entry.id}`,
                  () => client.removeConnection(entry.id)
                )}
              >
                {connectionBusy === `remove:${entry.id}` ? "Removing..." : "Remove"}
              </button>
            </>
          )}
        </SettingRow>
      ))}
      <div className="settingsButtonRow">
        <button type="button" onClick={() => {
          setWizardTarget(undefined);
          setWizardOpen(true);
        }}>
          <Plus size={13} /> Add model
        </button>
      </div>
      {!unconfigured && (
        <CredentialPanel
          status={credential}
          onSet={(secret) => client.setKeyringCredential(secret).then(onCredential)}
          onValidate={() => client.validateCredential().then(onCredential)}
          onClear={() => client.clearKeyringCredential().then(onCredential)}
          onError={onError}
        />
      )}
      <Presence open={wizardOpen} kind="dialog">
        {wizardOpen && (
          <ConnectionWizard
            snapshot={snapshot}
            client={client}
            connections={connections}
            target={wizardTarget}
            onModelAdded={onModelAdded}
            onClose={() => {
              setWizardOpen(false);
              setWizardTarget(undefined);
            }}
            onApplied={async () => {
              await refresh();
              if (unconfigured) onConfigured();
            }}
            onError={onError}
          />
        )}
      </Presence>
    </div>
  );
}

// ConnectionWizard: linear guided "Add model" flow — Base URL、Protocol、
// Model ID、API Key 四要素，单一提交。每条连接都是 OpenAI-compatible
// 端点；模型元数据由探测或手填提供。
// 提交语义自动判定：未配置时完成首次配置；默认连接上的新模型走热注册
// （model/add，不重建 Runtime）；其余端点建立新连接。
function ConnectionWizard({
  snapshot,
  client,
  connections,
  target,
  onClose,
  onApplied,
  onModelAdded,
  onError
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  connections?: ConnectionListResult;
  target?: ConnectionEntry;
  onClose: () => void;
  onApplied: () => Promise<void> | void;
  onModelAdded: (model: string, metadata: SetupModelMetadata) => void;
  onError: (error: unknown) => void;
}) {
  const unconfigured = snapshot.phase === "setup";
  const dialogRef = useRef<HTMLElement>(null);
  useModalFocus(dialogRef, true, onClose);
  const [modelID, setModelID] = useState(target?.model ?? "");
  const [baseURL, setBaseURL] = useState(target?.base_url ?? "");
  const [apiKey, setAPIKey] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [problem, setProblem] = useState("");
  const [manualMetadata, setManualMetadata] = useState(false);
  const [metadata, setMetadata] = useState<ModelMetadataDraft>(emptyModelMetadataDraft());
  const [detected, setDetected] = useState(false);
  const [protocol, setProtocol] = useState(target?.protocol || "openai_chat");
  const keyError = apiKeyError(apiKey);
  const [detecting, setDetecting] = useState(false);

  // 元数据恒必填：每条连接都是显式 OpenAI-compatible 端点。
  const metadataReady = detected || manualMetadata;
  // 探测结果信任 Provider 响应；仅手动填写的元数据做严格校验。
  const metadataProblem = manualMetadata
    ? modelMetadataProblem(metadata, protocol)
    : "";
  const sameBaseURL = (entry: ConnectionEntry) =>
    (entry.base_url ?? "").replace(/\/+$/, "") === baseURL.trim().replace(/\/+$/, "");
  const activeConnection = connections?.connections.find((entry) => entry.default);
  // 已存在同端点连接（编辑或热注册新模型）时凭证已保存，无需重输 API Key。
  const existingConnection = Boolean(target) ||
    (activeConnection !== undefined && sameBaseURL(activeConnection));
  const canSubmit = baseURL.trim() !== "" && modelID.trim() !== "" &&
    (apiKey.trim() !== "" || existingConnection) && !keyError &&
    metadataReady && !metadataProblem;

  const detect = async () => {
    if (detecting || !baseURL.trim() || !modelID.trim()) return;
    setDetecting(true);
    setProblem("");
    try {
      // 默认连接上的模型且未填新 Key 时走 Runtime 探测（复用会话凭证）；
      // 填写了 API Key 时一律走 Supervisor 探测——用户输入的 Key 优先
      // 于已保存凭证，覆盖"同端点换新 Key"的场景。
      const useRuntimeProbe = !unconfigured && !apiKey.trim() &&
        activeConnection !== undefined && sameBaseURL(activeConnection);
      const result = useRuntimeProbe
        ? await client.probeModel(modelID.trim())
        : await client.probeSetup({
            base_url: baseURL.trim(),
            protocol,
            model: modelID.trim(),
            ...(apiKey.trim() ? {api_key: apiKey.trim()} : {})
          });
      setMetadata(modelMetadataFromProbe(modelID.trim(), result));
      setDetected(true);
    } catch (error) {
      setDetected(false);
      setProblem(error instanceof Error ? error.message : String(error));
    } finally {
      setDetecting(false);
    }
  };

  const submit = async () => {
    if (!canSubmit || submitting) return;
    setSubmitting(true);
    setProblem("");
    try {
      const requestMetadata = setupModelMetadata(metadata);
      // 默认连接的同一端点上新模型：热注册附加模型。
      if (!unconfigured && activeConnection && !target &&
          sameBaseURL(activeConnection) &&
          activeConnection.model !== modelID.trim()) {
        await client.addModel({model: modelID.trim(), model_metadata: requestMetadata});
        onModelAdded(modelID.trim(), requestMetadata);
      } else if (unconfigured) {
        await client.completeSetup({
          model: modelID.trim(),
          api_key: apiKey,
          base_url: baseURL.trim(),
          protocol,
          model_metadata: requestMetadata
        });
      } else {
        await client.addConnection({
          model: modelID.trim(),
          api_key: apiKey,
          base_url: baseURL.trim(),
          protocol,
          model_metadata: requestMetadata
        });
      }
      await onApplied();
      onClose();
    } catch (error) {
      setProblem(error instanceof Error ? error.message : String(error));
      onError(error);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="modelEditorOverlay" data-motion-backdrop role="presentation">
      <section
        ref={dialogRef}
        className="modelEditorDialog connectionWizard"
        data-motion-surface
        role="dialog"
        aria-modal="true"
        aria-labelledby="connection-wizard-title"
      >
        <header>
          <h3 id="connection-wizard-title">
            {target ? "Edit connection" : unconfigured ? "Configure model" : "Add model"}
          </h3>
          <button
            type="button"
            className="settingsClose"
            aria-label="Close model editor"
            onClick={onClose}
          >
            <X size={15} />
          </button>
        </header>
        <div className="modelEditorBody">
          <label className="selectField">
            <span>1 · Base URL (HTTPS or loopback)</span>
            <input
              className="settingsSelect"
              aria-label="Connection base URL"
              value={baseURL}
              placeholder="https://api.example.com/v1"
              disabled={submitting || detecting}
              onChange={(event) => {
                setBaseURL(event.target.value);
                setDetected(false);
              }}
            />
          </label>
          <label className="selectField">
            <span>2 · Protocol</span>
            <SelectControl
              label="Connection protocol"
              value={protocol}
              values={["openai_chat", "openai_responses"]}
              disabled={submitting || detecting}
              format={(value) => value === "openai_chat" ? "Chat Completions" : "Responses"}
              onChange={(value) => {
                setProtocol(value);
                setDetected(false);
              }}
            />
          </label>
          <label className="selectField">
            <span>3 · Model ID</span>
            <input
              className="settingsSelect"
              aria-label="Connection model ID"
              value={modelID}
              placeholder="Enter the exact model ID"
              disabled={submitting || detecting}
              onChange={(event) => {
                setModelID(event.target.value);
                setMetadata(emptyModelMetadataDraft(event.target.value.trim()));
                setDetected(false);
              }}
            />
          </label>
          <label className="selectField">
            <span>4 · API Key</span>
            <input
              className="settingsSelect"
              type="password"
              autoComplete="off"
              aria-label="Connection API key"
              value={apiKey}
              placeholder={target ? "Leave blank to reuse the saved key" : "Paste the API key"}
              disabled={submitting || detecting}
              onChange={(event) => setAPIKey(event.target.value)}
            />
          </label>
          {!metadataReady && (
            <div className="settingsButtonRow">
              <button
                type="button"
                disabled={detecting || !baseURL.trim() || !modelID.trim()}
                onClick={() => void detect()}
              >
                {detecting ? "Detecting..." : "Detect model"}
              </button>
              <button type="button" onClick={() => {
                setManualMetadata(true);
                setMetadata(emptyModelMetadataDraft(modelID.trim()));
              }}>
                Enter model metadata
              </button>
            </div>
          )}
          {metadataReady && (
            <ModelMetadataFields
              value={metadata}
              disabled={submitting}
              onChange={setMetadata}
            />
          )}
          {keyError && (
            <small className="settingsError" role="alert">{keyError}</small>
          )}
          {problem && (
            <small className="settingsError" role="alert">{problem}</small>
          )}
        </div>
        <footer>
          <button type="button" disabled={submitting} onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            disabled={!canSubmit || submitting || detecting}
            onClick={() => void submit()}
          >
            {submitting
              ? <><RefreshCw className="spin" size={13} /> Saving...</>
              : unconfigured ? "Save and start" : target ? "Save connection" : "Add model"}
          </button>
        </footer>
      </section>
    </div>
  );
}

function ModelCapabilityPanel({model}: {model: ModelCatalogEntry}) {
  const capabilities = model.capabilities;
  const labels = [
    capabilities.streaming && "Streaming",
    capabilities.reasoning && "Reasoning",
    capabilities.tool_calls && "Tool calls",
    capabilities.parallel_tool_calls === "supported" && "Parallel tools",
    capabilities.vision && "Vision",
    capabilities.image_input && "Image input",
    capabilities.native_search && "Native search",
    capabilities.prompt_cache && "Prompt cache",
    capabilities.automatic_prompt_cache && "Automatic cache",
    capabilities.incremental_responses && "Incremental responses",
    capabilities.thinking_toggle && "Thinking toggle"
  ].filter((value): value is string => Boolean(value));
  return (
    <div className="settingsBlock modelCapabilityPanel">
      <div className="settingsBlockTitle">
        <span>{capabilities.display_name || model.id}</span>
        <small data-restart={
          capabilities.selection_mode === "restart_required" || undefined
        }>
          {capabilities.selection_mode.replaceAll("_", " ")}
        </small>
      </div>
      {capabilities.unavailable_reason && (
        <p>{capabilities.unavailable_reason}</p>
      )}
      <p role="status" className="settingsMetadataProvenance">
        Metadata: <code>{JSON.stringify(capabilities.metadata_provenance)}</code>
      </p>
      <dl className="settingsFacts">
        <div><dt>Context window</dt><dd>{capabilities.context_window.toLocaleString()}</dd></div>
        <div><dt>Max output</dt><dd>{capabilities.max_output_tokens.toLocaleString()}</dd></div>
      </dl>
      <div className="capabilityTags">
        {labels.map((label) => <span key={label}>{label}</span>)}
      </div>
    </div>
  );
}

function ToolSettings({
  snapshot,
  draft,
  onDraftChange,
  query,
  onQueryChange
}: {
  snapshot: RuntimeSnapshot;
  draft: ProfileDraft;
  onDraftChange: (patch: Partial<ProfileDraft>) => void;
  query: string;
  onQueryChange: (value: string) => void;
}) {
  const normalized = query.trim().toLocaleLowerCase();
  const tools = useMemo(() => snapshot.tools.filter((tool) =>
    !normalized || [tool.name, tool.description, tool.source_label]
      .some((value) => value.toLocaleLowerCase().includes(normalized))
  ), [normalized, snapshot.tools]);
  return (
    <SettingsSectionView
      title="Tools"
      description="Tools remain governed by Runtime policy, approval, and sandbox."
    >
      <label className="settingsSearch">
        <Search size={15} />
        <input
          type="search"
          aria-label="Search tools"
          placeholder="Search tools"
          value={query}
          onChange={(event) => onQueryChange(event.target.value)}
        />
      </label>
      <div className="settingsCatalogHeading">
        <strong>Catalog</strong><span>{tools.length}</span>
      </div>
      <div className="settingsCatalog">
        {tools.map((tool) => {
          const enabled = draft.enabledToolIDs.includes(tool.id);
          return (
            <div className="settingsCatalogRow settingsToolRow" key={tool.id}>
              <Wrench size={15} />
              <span>
                <strong>{tool.name}</strong>
                <small>{tool.description || tool.source_label}</small>
              </span>
              <span className="settingsTags">
                <small>{tool.source_label}</small>
                <small>{tool.risk_level}</small>
                {tool.guarded && <small>guarded</small>}
              </span>
              <Switch
                label={`${enabled ? "Disable" : "Enable"} ${tool.name}`}
                checked={enabled}
                disabled={
                  tool.availability !== "available" ||
                  !mutable(snapshot, "enabled_tool_ids") ||
                  (enabled && draft.enabledToolIDs.length === 1)
                }
                title={enabled && draft.enabledToolIDs.length === 1
                  ? "At least one tool must remain enabled"
                  : undefined}
                onChange={(next) => {
                  const ids = next
                    ? [...new Set([...draft.enabledToolIDs, tool.id])]
                    : draft.enabledToolIDs.filter((id) => id !== tool.id);
                  onDraftChange({enabledToolIDs: ids.sort()});
                }}
              />
              <details className="settingsCatalogDetails">
                <summary>Details</summary>
                <dl>
                  <div><dt>Source</dt><dd>{tool.source_kind} · {tool.source_label}</dd></div>
                  <div><dt>Capability</dt><dd>{tool.capability || "unknown"}</dd></div>
                  <div><dt>Access</dt><dd>{tool.access_mode || "unknown"}</dd></div>
                  <div><dt>Sandbox</dt><dd>{tool.sandbox_requirement || "unknown"}</dd></div>
                  <div>
                    <dt>Policy</dt>
                    <dd>{tool.policy_state || "unknown"} · {tool.policy_reason}</dd>
                  </div>
                  <div>
                    <dt>Constitution</dt>
                    <dd>
                      {tool.constitution_state || "unknown"} · {tool.constitution_reason}
                    </dd>
                  </div>
                  {tool.unavailable_reason && (
                    <div><dt>Error</dt><dd>{tool.unavailable_reason}</dd></div>
                  )}
                </dl>
              </details>
            </div>
          );
        })}
      </div>
    </SettingsSectionView>
  );
}

function ExtensionSettings({
  snapshot,
  client,
  onError
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  onError: (error: unknown) => void;
}) {
  const [pending, setPending] = useState("");
  const [details, setDetails] = useState<Record<string, string>>({});
  const [result, setResult] = useState("");
  const run = async (
    extension: RuntimeSnapshot["extensions"][number],
    action: "detail" | "verify" | "enable" | "disable"
  ) => {
    const key = `${extension.kind}:${extension.name}`;
    if (pending) return;
    setPending(`${key}:${action}`);
    setResult("");
    try {
      const value = action === "enable" || action === "disable"
        ? await client.setExtensionEnabled(
            extension.kind,
            extension.name,
            action === "enable"
          )
        : await client.controlExtension(
            extension.kind,
            extension.name,
            action
          );
      if (action === "detail" || value.detail) {
        setDetails((current) => ({
          ...current,
          [key]: JSON.stringify(
            value.detail ?? value.receipt ?? value.extensions ?? {},
            null,
            2
          )
        }));
      }
      setResult(
        `${titleCase(action)} ${value.receipt?.status || "completed"}`
      );
    } catch (operationError) {
      onError(operationError);
    } finally {
      setPending("");
    }
  };
  return (
    <SettingsSectionView
      title="Extensions"
      description="Installed skills visible to the Runtime."
    >
      {result && (
        <div className="settingsInlineResult" role="status">
          <Check size={13} /> {result}
        </div>
      )}
      {snapshot.extensions.length === 0 ? (
        <p className="settingsEmpty">No extensions are registered.</p>
      ) : (
        <div className="settingsCatalog settingsExtensionGrid">
          {snapshot.extensions.map((extension) => {
            const key = `${extension.kind}:${extension.name}`;
            return (
              <article className="settingsCatalogRow settingsExtension" key={key}>
                <Boxes size={15} />
                <span>
                  <strong>{extension.name}</strong>
                  <small>
                    {extension.kind}
                    {extension.version ? ` · ${extension.version}` : ""}
                    {extension.source ? ` · ${extension.source}` : ""}
                  </small>
                </span>
                <span className="settingsHealth" data-health={extension.health}>
                  <span />{extension.health}
                </span>
                <Switch
                  label={`${extension.enabled ? "Disable" : "Enable"} ${extension.name}`}
                  checked={extension.enabled}
                  disabled={Boolean(pending)}
                  onChange={(next) => void run(
                    extension,
                    next ? "enable" : "disable"
                  )}
                />
                <div className="settingsExtensionFacts">
                  <span>Trust: {extension.trust || "not declared"}</span>
                  {extension.publisher && (
                    <span>Publisher: {extension.publisher}</span>
                  )}
                  <span>
                    Permissions: {extension.permissions?.join(", ") || "none declared"}
                  </span>
                  <span>
                    Capabilities: {
                      extension.capabilities?.map((capability) =>
                        `${capability.id}${capability.enabled ? "" : " (off)"}`
                      ).join(", ") || "none declared"
                    }
                  </span>
                  {extension.digest && <span>Digest: {extension.digest.slice(0, 12)}</span>}
                </div>
                <div className="settingsButtonRow settingsExtensionActions">
                  <button
                    type="button"
                    disabled={Boolean(pending)}
                    onClick={() => void run(extension, "detail")}
                  >
                    Details
                  </button>
                  <button
                    type="button"
                    disabled={Boolean(pending)}
                    onClick={() => void run(extension, "verify")}
                  >
                    <ShieldCheck size={13} />
                    Verify
                  </button>
                </div>
                {details[key] && (
                  <pre className="settingsCode settingsExtensionDetail">
                    {details[key]}
                  </pre>
                )}
              </article>
            );
          })}
        </div>
      )}
    </SettingsSectionView>
  );
}

function AgentSettings({
  snapshot,
  client,
  draft,
  onDraftChange,
  onAppliedPreset,
  applyBlocked,
  onError
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  draft: ProfileDraft;
  onDraftChange: (patch: Partial<ProfileDraft>) => void;
  onAppliedPreset: (result: AgentPresetApplyResult) => void;
  applyBlocked: boolean;
  onError: (error: unknown) => void;
}) {
  const [presets, setPresets] = useState<AgentPreset[]>([]);
  const [selectedPresetID, setSelectedPresetID] = useState("");
  const [presetName, setPresetName] = useState("");
  const [presetDescription, setPresetDescription] = useState("");
  const [presetPending, setPresetPending] = useState("");
  const [presetResult, setPresetResult] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const selectedPreset = presets.find(
    (preset) => preset.id === selectedPresetID
  );

  useEffect(() => {
    let current = true;
    void client.listAgentPresets().then(
      (result) => {
        if (!current) return;
        setPresets(result.presets);
        setSelectedPresetID((selected) =>
          result.presets.some((preset) => preset.id === selected)
            ? selected
            : result.presets[0]?.id ?? ""
        );
      },
      onError
    );
    return () => {
      current = false;
    };
  }, [client, onError]);

  useEffect(() => {
    if (!selectedPreset) return;
    setPresetName(selectedPreset.name);
    setPresetDescription(selectedPreset.description ?? "");
    setConfirmDelete(false);
  }, [selectedPreset?.id, selectedPreset?.revision]);

  const runPreset = async (action: string, operation: () => Promise<void>) => {
    if (presetPending) return;
    setPresetPending(action);
    setPresetResult("");
    try {
      await operation();
      setPresetResult(action);
    } catch (operationError) {
      onError(operationError);
    } finally {
      setPresetPending("");
    }
  };
  const savePreset = async (
    id?: string,
    expectedRevision?: number,
    name = presetName
  ) => {
    const result = await client.saveAgentPreset({
      id,
      expectedRevision,
      name: name.trim(),
      description: presetDescription.trim(),
      profile: agentPresetProfile(draft)
    });
    if (!result.preset) return;
    setPresets((current) => [
      ...current.filter((preset) => preset.id !== result.preset?.id),
      result.preset!
    ].sort((left, right) => left.name.localeCompare(right.name)));
    setSelectedPresetID(result.preset.id);
  };
  return (
    <SettingsSectionView
      title="Agent preset"
      description="Compose reusable workspace presets and apply them to the active session."
    >
      <div className="settingsBlock presetWorkbench">
        <div className="settingsBlockTitle">
          <span>Workspace presets</span>
          <small>{presets.length}</small>
        </div>
        <div className="presetFields">
          <label>
            <span>Saved preset</span>
            <select
              className="settingsSelect"
              aria-label="Saved agent preset"
              value={selectedPresetID}
              onChange={(event) => {
                const id = event.target.value;
                setSelectedPresetID(id);
                if (!id) {
                  setPresetName("");
                  setPresetDescription("");
                }
              }}
            >
              <option value="">New preset</option>
              {presets.map((preset) => (
                <option value={preset.id} key={preset.id}>
                  {preset.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            <span>Name</span>
            <input
              aria-label="Agent preset name"
              value={presetName}
              maxLength={80}
              placeholder="Focused review"
              onChange={(event) => setPresetName(event.target.value)}
            />
          </label>
          <label>
            <span>Description</span>
            <input
              aria-label="Agent preset description"
              value={presetDescription}
              maxLength={512}
              placeholder="Optional"
              onChange={(event) => setPresetDescription(event.target.value)}
            />
          </label>
        </div>
        <div className="presetScope">
          <span>Scope</span>
          <strong>Workspace</strong>
          <small>Apply target: current Session</small>
        </div>
        <div className="settingsButtonRow">
          <button
            type="button"
            disabled={!presetName.trim() || Boolean(presetPending)}
            onClick={() => void runPreset(
              "Preset created",
              () => savePreset()
            )}
          >
            <Save size={13} /> Save new
          </button>
          <button
            type="button"
            disabled={!selectedPreset || !presetName.trim() || Boolean(presetPending)}
            onClick={() => selectedPreset && void runPreset(
              "Preset updated",
              () => savePreset(selectedPreset.id, selectedPreset.revision)
            )}
          >
            <RefreshCw size={13} /> Update
          </button>
          <button
            type="button"
            disabled={!selectedPreset || Boolean(presetPending)}
            onClick={() => selectedPreset && void runPreset(
              "Preset copied",
              () => savePreset(
                undefined,
                undefined,
                uniquePresetName(`${selectedPreset.name} copy`, presets)
              )
            )}
          >
            <Copy size={13} /> Duplicate
          </button>
          <button
            type="button"
            disabled={!selectedPreset || Boolean(presetPending)}
            onClick={() => selectedPreset && void runPreset(
              "Preset loaded",
              async () => {
                onDraftChange(profileDraftFromPreset(
                  selectedPreset.profile,
                  snapshot.tools
                ));
              }
            )}
          >
            Load into draft
          </button>
          <button
            type="button"
            disabled={!selectedPreset || Boolean(presetPending) || applyBlocked}
            title={applyBlocked
              ? "Finish the active Turn before applying a preset"
              : "Apply preset to current Session"}
            onClick={() => selectedPreset && void runPreset(
              "Preset applied",
              async () => {
                const result = await client.applyAgentPreset(selectedPreset.id);
                onAppliedPreset(result);
              }
            )}
          >
            <Check size={13} /> Apply to session
          </button>
          {confirmDelete ? (
            <>
              <button type="button" onClick={() => setConfirmDelete(false)}>
                Keep preset
              </button>
              <button
                type="button"
                className="settingsDanger"
                disabled={!selectedPreset || Boolean(presetPending)}
                onClick={() => selectedPreset && void runPreset(
                  "Preset deleted",
                  async () => {
                    await client.deleteAgentPreset(selectedPreset);
                    setPresets((current) => current.filter(
                      (preset) => preset.id !== selectedPreset.id
                    ));
                    setSelectedPresetID("");
                    setPresetName("");
                    setPresetDescription("");
                  }
                )}
              >
                <Trash2 size={13} /> Confirm delete
              </button>
            </>
          ) : (
            <button
              type="button"
              className="settingsDanger"
              disabled={!selectedPreset || Boolean(presetPending)}
              onClick={() => setConfirmDelete(true)}
            >
              <Trash2 size={13} /> Delete
            </button>
          )}
        </div>
        {presetResult && (
          <div className="settingsInlineResult" role="status">
            <Check size={13} /> {presetResult}
          </div>
        )}
      </div>
      <SettingRow title="Mode" description="Plan before acting or execute the requested task.">
        <SelectControl
          label="Agent mode"
          value={draft.mode}
          values={["plan", "act", "operate"]}
          disabled={!mutable(snapshot, "mode")}
          onChange={(mode) => onDraftChange({
            mode: mode as ProfileDraft["mode"]
          })}
        />
      </SettingRow>
      <SettingRow title="Approval" description="Control when consequential actions ask first.">
        <SelectControl
          label="Approval posture"
          value={draft.approvalPosture}
          values={["suggest", "auto", "never"]}
          disabled={!mutable(snapshot, "approval_posture")}
          onChange={(approvalPosture) => onDraftChange({approvalPosture})}
        />
      </SettingRow>
      <SettingRow title="Execution" description="Choose the execution isolation target.">
        <SelectControl
          label="Execution target"
          value={draft.executionTarget}
          values={["local"]}
          disabled={!mutable(snapshot, "execution_target")}
          onChange={(executionTarget) => onDraftChange({executionTarget})}
        />
      </SettingRow>
      <NumberSetting
        value={draft.maxSteps}
        disabled={!mutable(snapshot, "max_steps")}
        onChange={(maxSteps) => onDraftChange({maxSteps})}
      />
      {(snapshot.agents.length > 0 || snapshot.usage) && (
        <div className="settingsBlock">
          <div className="settingsBlockTitle">Current activity</div>
          {snapshot.usage && (
            <dl className="settingsFacts" aria-label="Usage">
              <div><dt>Turns</dt><dd>{snapshot.usage.turns}</dd></div>
              <div><dt>Calls</dt><dd>{snapshot.usage.calls}</dd></div>
              <div><dt>Tokens</dt><dd>{snapshot.usage.total_tokens.toLocaleString()}</dd></div>
            </dl>
          )}
          <div className="settingsActivity">
            {snapshot.agents.map((agent) => (
              <div key={agent.id}>
                <span>
                  <strong>{agent.role}</strong>
                  <small>{agent.id}</small>
                </span>
                <b>{agent.status}</b>
                {agent.last_message && <small>{agent.last_message}</small>}
              </div>
            ))}
          </div>
        </div>
      )}
      {snapshot.checkpoints.length > 0 && (
        <div className="settingsBlock">
          <div className="settingsBlockTitle">Recovery checkpoints</div>
          <div className="checkpointList">
            {snapshot.checkpoints.map((checkpoint) => (
              <div key={checkpoint.id}>
                <span title={checkpoint.summary}>{checkpoint.summary}</span>
                <button
                  type="button"
                  disabled={!checkpoint.can_restore}
                  onClick={() => void client.restoreCheckpoint(checkpoint.id).catch(onError)}
                >
                  Restore
                </button>
                <button
                  type="button"
                  disabled={!checkpoint.can_fork}
                  onClick={() => void client.forkCheckpoint(checkpoint.id).catch(onError)}
                >
                  Fork
                </button>
              </div>
            ))}
          </div>
        </div>
      )}
    </SettingsSectionView>
  );
}

function NumberSetting({
  value,
  disabled,
  onChange
}: {
  value: number;
  disabled: boolean;
  onChange: (value: number) => void;
}) {
  return (
    <SettingRow
      title="Maximum steps"
      description="Consecutive no-progress steps allowed before finalization."
    >
      <input
        className="settingsNumber"
        aria-label="Maximum steps"
        type="number"
        min={0}
        value={value}
        disabled={disabled}
        onChange={(event) => {
          const next = Number(event.target.value);
          if (Number.isInteger(next) && next >= 0) onChange(next);
        }}
      />
    </SettingRow>
  );
}

function CredentialPanel({
  status,
  onSet,
  onValidate,
  onClear,
  onError
}: {
  status?: CredentialStatus;
  onSet: (secret: string) => Promise<unknown>;
  onValidate: () => Promise<unknown>;
  onClear: () => Promise<unknown>;
  onError: (error: unknown) => void;
}) {
  const [secret, setSecret] = useState("");
  const [pending, setPending] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const [result, setResult] = useState("");
  const run = async (
    operation: () => Promise<unknown>,
    success: string,
    clear = false
  ) => {
    if (pending) return;
    setPending(true);
    setResult("");
    try {
      await operation();
      if (clear) setSecret("");
      setConfirmClear(false);
      setResult(success);
    } catch (error) {
      onError(error);
    } finally {
      setPending(false);
    }
  };
  return (
    <div className="settingsBlock credentialPanel">
      <div className="settingsBlockTitle">
        <span><KeyRound size={15} /> Credential</span>
        <span className="settingsHealth" data-health={
          status?.configured ? status.validation : "missing"
        }>
          <span />
          {status?.configured ? status.validation : "missing"}
        </span>
      </div>
      <p>
        Stored in {status?.reference.kind || "secure external storage"}.
        Secret values are never returned to the browser.
      </p>
      {status?.reference.name && (
        <p className="settingsReference">Reference: {status.reference.name}</p>
      )}
      {status?.validation_detail && <p>{status.validation_detail}</p>}
      {status?.validated_at && (
        <p>Validated {new Date(status.validated_at).toLocaleString()}</p>
      )}
      {status?.restart_required && (
        <div className="settingsInlineResult" data-tone="warning">
          <RefreshCw size={13} /> Runtime restart required
        </div>
      )}
      <div className="credentialInput">
        <input
          type="password"
          autoComplete="off"
          aria-label="Provider credential"
          placeholder="API key"
          value={secret}
          onChange={(event) => setSecret(event.target.value)}
        />
        <button
          type="button"
          disabled={!secret.trim() || pending}
          onClick={() => void run(
            () => onSet(secret),
            status?.configured ? "Credential rotated" : "Credential created",
            true
          )}
        >
          {status?.configured ? "Rotate key" : "Set key"}
        </button>
      </div>
      <div className="settingsButtonRow">
        <button
          type="button"
          disabled={!status?.configured || pending}
          onClick={() => void run(onValidate, "Connection verified")}
        >
          <RefreshCw className={pending ? "spin" : undefined} size={13} />
          {pending ? "Testing..." : "Test connection"}
        </button>
        {status?.reference.kind === "keyring" && (
          confirmClear ? (
            <>
              <button type="button" onClick={() => setConfirmClear(false)}>
                Keep key
              </button>
              <button
                type="button"
                className="settingsDanger"
                disabled={!status.configured || pending}
                onClick={() => void run(
                  onClear,
                  "Credential removed from Keyring"
                )}
              >
                Confirm clear
              </button>
            </>
          ) : (
            <button
              type="button"
              disabled={!status.configured || pending}
              onClick={() => setConfirmClear(true)}
            >
              Clear key
            </button>
          )
        )}
      </div>
      {result && (
        <div className="settingsInlineResult" role="status">
          <Check size={13} /> {result}
        </div>
      )}
    </div>
  );
}

function SettingsSectionView({
  title,
  description,
  children
}: {
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <section className="settingsSection">
      <h3>{title}</h3>
      <p className="settingsIntro">{description}</p>
      <div className="settingsRows">{children}</div>
    </section>
  );
}

function SettingRow({
  title,
  description,
  children
}: {
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <div className="settingsRow">
      <span>
        <strong>{title}</strong>
        <small>{description}</small>
      </span>
      {children}
    </div>
  );
}

function SelectControl({
  label,
  value,
  values,
  disabled,
  format = titleCase,
  onChange
}: {
  label: string;
  value: string;
  values: readonly string[];
  disabled?: boolean;
  format?: (value: string) => string;
  onChange: (value: string) => void;
}) {
  return (
    <label className="settingsSelectWrap">
      <select
        className="settingsSelect"
        aria-label={label}
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
      >
        {values.map((option) => (
          <option key={option} value={option}>{format(option)}</option>
        ))}
      </select>
      <ChevronDown size={15} />
    </label>
  );
}

function ThemeChoice({
  label,
  icon,
  selected,
  onClick
}: {
  label: string;
  icon: ReactNode;
  selected: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onClick}
    >
      {icon}<span>{label}</span>
    </button>
  );
}

function mutable(snapshot: RuntimeSnapshot, field: keyof SessionProfile): boolean {
  return snapshot.profile?.capabilities.mutable_fields.includes(field) ?? false;
}

function titleCase(value: string): string {
  return value
    .replaceAll("_", " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function settingsProfileDraft(snapshot: RuntimeSnapshot): ProfileDraft {
  const profile = snapshot.profile?.profile;
  return settingsProfileDraftFromProfile(profile ?? {
    version: 1,
    revision: 1,
    mode: "act",
    planning_policy: "adaptive",
    provider: "",
    model: "",
    approval_posture: "auto",
    execution_target: "local",
    max_steps: 64,
    prompt_cache_revision: 1
  }, snapshot.tools);
}

function settingsProfileDraftFromProfile(
  profile: SessionProfile,
  tools: RuntimeSnapshot["tools"]
): ProfileDraft {
  const enabledToolIDs = profile.enabled_tool_ids?.length
    ? [...profile.enabled_tool_ids]
    : tools.filter((tool) => tool.enabled).map((tool) => tool.id);
  return {
    mode: profile.mode,
    provider: profile.provider,
    model: profile.model,
    reasoningEffort: profile.reasoning_effort ?? "",
    enabledToolIDs: enabledToolIDs.sort(),
    approvalPosture: profile.approval_posture,
    executionTarget: profile.execution_target,
    maxSteps: profile.max_steps
  };
}

function equalProfileDraft(
  left: ProfileDraft,
  right: ProfileDraft
): boolean {
  return left.mode === right.mode &&
    left.provider === right.provider &&
    left.model === right.model &&
    left.reasoningEffort === right.reasoningEffort &&
    left.approvalPosture === right.approvalPosture &&
    left.executionTarget === right.executionTarget &&
    left.maxSteps === right.maxSteps &&
    arraysEqual(left.enabledToolIDs, right.enabledToolIDs);
}

function modelIDProblem(modelID: string): string {
  const value = modelID.trim();
  if (!value) return "Model ID is required";
  if (value.length > 256) return "Model ID must be 256 characters or fewer";
  if (/\s/.test(value)) return "Model ID cannot contain whitespace";
  return "";
}

function changedProfileFields(
  baseline: ProfileDraft,
  draft: ProfileDraft
): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  if (draft.mode !== baseline.mode) {
    patch.mode = draft.mode;
  }
  if (draft.provider !== baseline.provider) patch.provider = draft.provider;
  if (draft.model !== baseline.model) patch.model = draft.model;
  if (draft.reasoningEffort !== baseline.reasoningEffort) {
    patch.reasoning_effort = draft.reasoningEffort;
  }
  if (draft.approvalPosture !== baseline.approvalPosture) {
    patch.approval_posture = draft.approvalPosture;
  }
  if (draft.executionTarget !== baseline.executionTarget) {
    patch.execution_target = draft.executionTarget;
  }
  if (draft.maxSteps !== baseline.maxSteps) patch.max_steps = draft.maxSteps;
  if (
    draft.enabledToolIDs.length !== baseline.enabledToolIDs.length ||
    draft.enabledToolIDs.some((id, index) => id !== baseline.enabledToolIDs[index])
  ) {
    patch.enabled_tool_ids = draft.enabledToolIDs;
  }
  return patch;
}

function agentPresetProfile(draft: ProfileDraft): AgentPresetProfile {
  return {
    mode: draft.mode,
    planning_policy: "adaptive",
    provider: draft.provider,
    model: draft.model,
    reasoning_effort: draft.reasoningEffort,
    enabled_tool_ids: [...draft.enabledToolIDs],
    approval_posture: draft.approvalPosture,
    execution_target: draft.executionTarget,
    max_steps: draft.maxSteps
  };
}

function profileDraftFromPreset(
  profile: AgentPresetProfile,
  tools: RuntimeSnapshot["tools"]
): ProfileDraft {
  return {
    mode: profile.mode,
    provider: profile.provider,
    model: profile.model,
    reasoningEffort: profile.reasoning_effort ?? "",
    enabledToolIDs: profile.enabled_tool_ids?.length
      ? [...profile.enabled_tool_ids].sort()
      : tools.filter((tool) => tool.enabled).map((tool) => tool.id).sort(),
    approvalPosture: profile.approval_posture,
    executionTarget: profile.execution_target,
    maxSteps: profile.max_steps
  };
}

function profileApplyNotice(
  result: SessionProfileUpdateResult,
  before?: ProfileDraft,
  after?: ProfileDraft
): ApplyNotice {
  const changes = before && after ? [
    before.model !== after.model && `Model ${before.model} → ${after.model}`,
    before.mode !== after.mode && `Mode ${before.mode} → ${after.mode}`,
    before.reasoningEffort !== after.reasoningEffort &&
      `Reasoning ${before.reasoningEffort || "default"} → ${
        after.reasoningEffort || "default"
      }`,
    before.approvalPosture !== after.approvalPosture &&
      `Approval ${before.approvalPosture} → ${after.approvalPosture}`,
    before.maxSteps !== after.maxSteps && `Maximum steps ${after.maxSteps}`,
    !arraysEqual(before.enabledToolIDs, after.enabledToolIDs) && "Tools updated"
  ].filter((value): value is string => Boolean(value)) : [];
  const cache = result.prompt_cache_reset ? " · Prompt cache reset" : "";
  return {
    tone: "success",
    text: `${changes.length ? `Applied: ${changes.join("; ")}` : "Applied to current Session"}${cache}`
  };
}

function arraysEqual(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length &&
    left.every((value, index) => value === right[index]);
}

function uniquePresetName(
  candidate: string,
  presets: readonly AgentPreset[]
): string {
  const names = new Set(presets.map((preset) => preset.name.toLocaleLowerCase()));
  if (!names.has(candidate.toLocaleLowerCase())) return candidate;
  for (let suffix = 2; suffix <= presets.length + 2; suffix += 1) {
    const value = `${candidate} ${suffix}`;
    if (!names.has(value.toLocaleLowerCase())) return value;
  }
  return `${candidate} ${Date.now()}`;
}
