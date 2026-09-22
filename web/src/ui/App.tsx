import {
  AlertTriangle,
  ArrowDown,
  Archive,
  Braces,
  Check,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  CirclePause,
  CircleStop,
  FileCode2,
  FolderPlus,
  FolderOpen,
  GitFork,
  GitBranch,
  LoaderCircle,
  ListPlus,
  KeyRound,
  MessageSquarePlus,
  MoreHorizontal,
  Paperclip,
  PanelLeftClose,
  PanelLeftOpen,
  Menu,
  Pencil,
  Pin,
  PinOff,
  Plus,
  Play,
  RefreshCw,
  RotateCcw,
  Search,
  Send,
  Settings2,
  TextSelect,
  Trash2,
  Zap,
  Wrench,
  X
} from "lucide-react";
import {
  lazy,
  memo,
  Suspense,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode
} from "react";
import {ExecutionStages} from "./ExecutionStages";
import {Collapse} from "./primitives/Collapse";
import {IconButton} from "./primitives/IconButton";
import {Skeleton} from "./primitives/Skeleton";
import {useMediaQuery} from "./primitives/useMediaQuery";
import {useModalFocus} from "./primitives/useModalFocus";
import {TurnWithdrawalAction} from "./TurnWithdrawalAction";
import {Presence} from "./primitives/Presence";
import {useMotionEnabled} from "./primitives/motion";
import {usePresentationEvents} from "./usePresentationEvents";
import type {
  RuntimeEvent,
  SessionCheckpoint,
  SessionSummary,
  SetupCatalog,
  SetupRequest
} from "../protocol";
import {
  projectEditPlan,
  projectConversation,
  type ConversationNode
} from "../projection/conversation";
import {RuntimeClient, type RuntimeSnapshot} from "../runtime/client";
import {CapybaraMark} from "./brand/CapybaraMark";
import {QCodeWordmark} from "./brand/QCodeWordmark";
import {
  compactSelectWidth,
  ContextMeter,
  MessageActions,
  type ContextAttribution,
  type MessageChrome,
  type MessageFeedbackRating
} from "./ConversationChrome";
import type {ComposerCommand} from "./ComposerCommandMenu";
import {experience} from "./experience";
import {InputOptionMenu} from "./InputOptionMenu";
import {
  emptyModelMetadataDraft,
  ModelMetadataFields,
  modelMetadataFromProbe,
  modelMetadataProblem,
  setupModelMetadata
} from "./ModelMetadataFields";
import {ReasoningMenu} from "./ReasoningMenu";
import type {SettingsSection, ThemeMode} from "./SettingsDialog";
import type {BackgroundActivityTarget} from "./backgroundActivity";
import {
  AgentDisclosure,
  EditPlanPreview,
  ReasoningDisclosure,
  ToolDisclosure
} from "./TranscriptCards";
import {
  maxComposerAttachmentBytes,
  maxComposerAttachments,
  composerAttachmentAccept
} from "./attachmentLimits";
import type {
  ComposerAttachment,
  ComposerAttachmentSource
} from "./attachmentPipeline";
import {
  adjacentQuestion,
  projectConversationNavigation,
  questionPosition,
  transcriptPageForEntry,
  type ConversationNavigationItem
} from "./conversationNavigation";

export {selectionRange} from "./WorkspaceContextDialog";

interface Props {
  client: RuntimeClient;
}

const transcriptPageSize = 200;
const transcriptPageOverlap = 32;
const transcriptPageStep = transcriptPageSize - transcriptPageOverlap;
const compactCountFormat = new Intl.NumberFormat("en", {
  notation: "compact",
  maximumFractionDigits: 1
});
const Trajectory = lazy(async () => ({
  default: (await import("./Trajectory")).Trajectory
}));
const SettingsDialog = lazy(async () => ({
  default: (await import("./SettingsDialog")).SettingsDialog
}));
const WorkspaceContextDialog = lazy(async () => ({
  default: (await import("./WorkspaceContextDialog")).WorkspaceContextDialog
}));
const ComposerAttachments = lazy(async () => ({
  default: (await import("./ComposerAttachments")).ComposerAttachments
}));
const ComposerCommandMenu = lazy(async () => ({
  default: (await import("./ComposerCommandMenu")).ComposerCommandMenu
}));
const SessionProgress = lazy(async () => ({
  default: (await import("./SessionProgress")).SessionProgress
}));
const TurnQueue = lazy(async () => ({
  default: (await import("./TurnQueue")).TurnQueue
}));
const BackgroundActivityMonitor = lazy(async () => ({
  default: (await import("./BackgroundActivityMonitor")).BackgroundActivityMonitor
}));
const ConversationNavigator = lazy(async () => ({
  default: (await import("./ConversationNavigator")).ConversationNavigator
}));
const GitTools = lazy(async () => ({
  default: (await import("./GitTools")).GitTools
}));
const MarkdownMessage = lazy(async () => ({
  default: (await import("./MarkdownMessage")).MarkdownMessage
}));

interface TranscriptReadingPosition {
  readonly entryID: string;
  readonly top: number;
  readonly scrollTop: number;
  readonly windowEndID?: string;
  readonly atBottom: boolean;
}

interface TranscriptNavigationTarget {
  readonly entryID: string;
  readonly path?: string;
}

function initialRailCollapsed(): boolean {
  return readPreference("ch.sidebar.collapsed") === "true";
}

function initialSessionIsolation(): "shared" | "worktree" {
  return readPreference("ch.session.isolation") === "worktree"
    ? "worktree"
    : "shared";
}

function storedPanelWidth(key: string, fallback: number): number {
  const stored = readPreference(key);
  if (stored === null) return fallback;
  const value = Number(stored);
  return Number.isFinite(value) ? value : fallback;
}

function readPreference(key: string): string | null {
  try {
    return window.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

function writePreference(key: string, value: string): void {
  try {
    window.localStorage?.setItem(key, value);
  } catch {
    // Browser preferences are optional and never affect Runtime state.
  }
}

export function App({client}: Props) {
  const snapshot = useSyncExternalStore(
    client.subscribe,
    client.getSnapshot,
    client.getSnapshot
  );
  const query = snapshot.sessionSearchQuery;
  const [sessionSearchOpen, setSessionSearchOpen] = useState(false);
  const [collapsedWorkspaceIDs, setCollapsedWorkspaceIDs] =
    useState<ReadonlySet<string>>(() => new Set());
  const [workspaceDialogOpen, setWorkspaceDialogOpen] = useState(false);
  const [workspaceRemovalID, setWorkspaceRemovalID] = useState("");
  const [workspaceRemoving, setWorkspaceRemoving] = useState(false);
  const [draft, setDraft] = useState("");
  const [contextOpen, setContextOpen] = useState(false);
  const [railCollapsed, setRailCollapsed] = useState(initialRailCollapsed);
  const [mobileRailOpen, setMobileRailOpen] = useState(false);
  const [gitOpen, setGitOpen] = useState(true);
  const restoreGitFocus = useRef(false);
  useEffect(() => {
    if (!gitOpen && restoreGitFocus.current) {
      restoreGitFocus.current = false;
      document.querySelector<HTMLButtonElement>('[aria-controls="git-tools"]')?.focus();
    }
  }, [gitOpen]);
  const motionEnabled = useMotionEnabled();
  const compactViewport = useMediaQuery(`(max-width: ${experience.layout.compactBreakpoint}px)`);
  const railRef = useRef<HTMLElement>(null);
  useModalFocus(railRef, compactViewport && mobileRailOpen, () => setMobileRailOpen(false));
  useEffect(() => {
    setMobileRailOpen(false);
  }, [snapshot.selectedSessionID, snapshot.selectedWorkspaceID]);
  useEffect(() => {
    setGitOpen(true);
  }, [snapshot.selectedWorkspaceID]);
  const [railWidth, setRailWidth] = useState(
    () => storedPanelWidth(
      "ch.sidebar.width",
      experience.layout.sidebarDefault
    )
  );
  const [activeView, setActiveView] = useState<"chat" | "trajectory">("chat");
  const [inspectCallID, setInspectCallID] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [settingsSection, setSettingsSection] =
    useState<SettingsSection>("general");
  const [settingsAddModel, setSettingsAddModel] = useState(false);
  const [profilePending, setProfilePending] = useState("");
  const [cancelingTurnID, setCancelingTurnID] = useState("");
  const [themeMode, setThemeMode] = useState<ThemeMode>(readThemeMode);
  const [activityTarget, setActivityTarget] =
    useState<BackgroundActivityTarget>();
  const [submitting, setSubmitting] = useState(false);
  const [creatingSession, setCreatingSession] = useState(false);
  const [creatingWorkspaceID, setCreatingWorkspaceID] = useState("");
  const [localError, setLocalError] = useState("");
  const [composerAttachments, setComposerAttachments] =
    useState<ComposerAttachment[]>([]);
  const [draggingAttachment, setDraggingAttachment] = useState(false);
  const [commandMenuOpen, setCommandMenuOpen] = useState(false);
  const [commandQuery, setCommandQuery] = useState("");
  const [commandMenuSource, setCommandMenuSource] =
    useState<"button" | "slash">("button");
  const [sessionAction, setSessionAction] = useState<{
    sessionID: string;
    pending: boolean;
    error: string;
  }>();
  const [newIsolation, setNewIsolation] =
    useState<"shared" | "worktree">(initialSessionIsolation);
  const [transcriptWindowEndID, setTranscriptWindowEndID] = useState<string>();
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyError, setHistoryError] = useState("");
  const historyLoadRef = useRef<object>();
  const historyTopRef = useRef<HTMLDivElement>(null);
  const historyBottomRef = useRef<HTMLDivElement>(null);
  const scrollTopRef = useRef(0);
  const scrollDirectionRef = useRef<-1 | 0 | 1>(0);
  const readAndAdvanceRef = useRef<() => void>(() => {});
  const [conversationNavigatorOpen, setConversationNavigatorOpen] =
    useState(false);
  const [readerEntryID, setReaderEntryID] = useState("");
  const [navigationHighlightID, setNavigationHighlightID] = useState("");
  const [navigationTarget, setNavigationTarget] =
    useState<TranscriptNavigationTarget>();
  const transcriptRef = useRef<HTMLDivElement>(null);
  const transcriptContentRef = useRef<HTMLDivElement>(null);
  const readingPositionsRef =
    useRef(new Map<string, TranscriptReadingPosition>());
  const pendingReadingRestoreRef = useRef<TranscriptReadingPosition>();
  const interactionAnchorRef = useRef<{element: Element; top: number}>();
  const readerFrameRef = useRef<number>();
  const navigationReaderLockRef = useRef("");
  const navigationReaderLockTimerRef = useRef<number>();
  const navigationHighlightTimerRef = useRef<number>();
  const atBottomRef = useRef(true);
  const [atBottom, setAtBottom] = useState(true);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const attachmentInputRef = useRef<HTMLInputElement>(null);
  const composingRef = useRef(false);
  const attachmentGenerationRef = useRef(0);
  const cancelPendingRef = useRef("");
  const removedAttachmentIDs = useRef(new Set<string>());
  const attachmentsRef = useRef(composerAttachments);
  const selectedSessionRef = useRef(snapshot.selectedSessionID);
  const draftRef = useRef(draft);
  selectedSessionRef.current = snapshot.selectedSessionID;
  draftRef.current = draft;
  attachmentsRef.current = composerAttachments;
  const selected = snapshot.sessions.find(
    (item) => item.session_id === snapshot.selectedSessionID
  );
  const selectedWorkspace = snapshot.workspaces.find(
    (workspace) =>
      workspace.id === snapshot.selectedWorkspaceID && workspace.ready
  );
  const gitWorkspaceBusy = snapshot.sessions.some((item) =>
    item.workspace_root === selectedWorkspace?.root && sessionIsBusy(item)
  );
  const gitMutationDisabled = selected && selected.isolation !== "shared"
      ? "Workspace actions require a shared session."
      : gitWorkspaceBusy
        ? "Finish active work before changing Git state."
        : selected?.status === "blocked" || selected?.status === "interrupted"
          ? "Resume or resolve the interrupted turn first."
          : snapshot.profile?.profile.mode === "plan" || snapshot.profile?.profile.approval_posture === "never"
            ? "Git changes are unavailable in read-only mode."
            : selected?.archived ? "This session is archived." : "";
  const workspaceRemoval = snapshot.workspaces.find(
    (workspace) => workspace.id === workspaceRemovalID && workspace.removable
  );
  const projectedEntries = useMemo(
    () => snapshot.conversation.order.flatMap((id) => {
      const node = snapshot.conversation.nodes.get(id);
      return node ? [node] : [];
    }),
    [snapshot.conversation]
  );
  const entries = useMemo(
    () => projectedEntries.filter((entry) =>
      entry.kind !== "receipt" && entry.kind !== "deliverables"
    ),
    [projectedEntries]
  );
  const presentationEvents = usePresentationEvents(snapshot.events);
  const terminalTurns = useMemo(
    () => terminalTurnKinds(presentationEvents),
    [presentationEvents]
  );
  const resumableTurnID = !selected?.latest_turn_withdrawn &&
      (selected?.status === "blocked" || selected?.status === "interrupted")
    ? selected.latest_turn_id
    : "";
  const windowEndIndex = !atBottom && transcriptWindowEndID
    ? entries.findIndex((entry) => entry.id === transcriptWindowEndID)
    : -1;
  const transcriptEnd = windowEndIndex >= 0 ? windowEndIndex + 1 : entries.length;
  const transcriptStart = Math.max(0, transcriptEnd - transcriptPageSize);
  const visibleEntries = entries.slice(transcriptStart, transcriptEnd);
  const conversationNavigation = useMemo(
    () => projectConversationNavigation(entries),
    [entries]
  );
  const readerEntryIndex = useMemo(
    () => entries.findIndex((entry) => entry.id === readerEntryID),
    [entries, readerEntryID]
  );
  const currentQuestion = useMemo(
    () => questionPosition(
      conversationNavigation,
      readerEntryID,
      readerEntryIndex < 0 ? undefined : readerEntryIndex
    ),
    [conversationNavigation, readerEntryID, readerEntryIndex]
  );
  const previousQuestion = useMemo(
    () => adjacentQuestion(
      conversationNavigation,
      readerEntryID,
      -1,
      readerEntryIndex < 0 ? undefined : readerEntryIndex
    ),
    [conversationNavigation, readerEntryID, readerEntryIndex]
  );
  const nextQuestion = useMemo(
    () => adjacentQuestion(
      conversationNavigation,
      readerEntryID,
      1,
      readerEntryIndex < 0 ? undefined : readerEntryIndex
    ),
    [conversationNavigation, readerEntryID, readerEntryIndex]
  );
  const pendingApproval = snapshot.conversation.pendingApproval;
  const pendingInput = snapshot.conversation.pendingInput;
  const pendingApprovalKey = pendingRequestKey(snapshot.selectedSessionID, pendingApproval);
  const pendingInputKey = pendingRequestKey(snapshot.selectedSessionID, pendingInput);
  const activeTurn = snapshot.conversation.activeTurnID;
  useEffect(() => {
    if (cancelPendingRef.current && cancelPendingRef.current !== activeTurn) {
      cancelPendingRef.current = "";
      setCancelingTurnID("");
    }
  }, [activeTurn]);
  const traceRefreshSequence = presentationEvents.at(-1)?.sequence ?? 0;
  const selectedProvider = snapshot.profile?.profile.provider ?? "";
  const selectedModel = snapshot.profile?.profile.model ?? "";
  const selectedModelEntry = snapshot.models.find(
    (model) =>
      model.provider === selectedProvider &&
      model.id === selectedModel
  );
  const modelOptions = snapshot.models
    .filter((model) => model.provider === selectedProvider)
    .map((model) => ({
      value: model.id,
      label: model.id,
      disabled: model.capabilities.availability !== "available"
    }));
  const advertisedReasoningValues =
    selectedModelEntry?.capabilities.reasoning_efforts ?? [];
  const reasoningValues = selectedModelEntry?.capabilities.default_reasoning_effort
    ? advertisedReasoningValues
    : ["", ...advertisedReasoningValues];
  const latestReceipt = [...projectedEntries].reverse().find(
    (entry): entry is Extract<ConversationNode, {kind: "receipt"}> =>
      entry.kind === "receipt"
  );
  const turnChrome = useMemo(
    () => projectMessageChrome(presentationEvents),
    [presentationEvents]
  );
  const contextAttribution = useMemo(
    () => latestContextAttribution(presentationEvents),
    [presentationEvents]
  );
  const blankSession = Boolean(
    selected && entries.length === 0 && !snapshot.hydratingSessionID
  );
  const reportLocalError = useCallback((error: unknown) => {
    setLocalError(error instanceof Error ? error.message : String(error));
  }, []);
  const requestCancel = useCallback((turnID: string) => {
    if (!turnID || cancelPendingRef.current === turnID) return;
    cancelPendingRef.current = turnID;
    setCancelingTurnID(turnID);
    void client.cancel(turnID).catch((error) => {
      cancelPendingRef.current = "";
      setCancelingTurnID("");
      reportLocalError(error);
    });
  }, [client, reportLocalError]);
  const updateComposerProfile = useCallback(async (
    patch: Record<string, unknown>,
    label: string
  ) => {
    if (profilePending) return;
    setProfilePending(label);
    setLocalError("");
    try {
      await client.updateProfile(patch);
    } catch (error) {
      reportLocalError(error);
    } finally {
      setProfilePending("");
    }
  }, [client, profilePending, reportLocalError]);
  const captureReadingPosition = useCallback((includeNavigationLock = false) => {
    if (activeView !== "chat" || !snapshot.selectedSessionID) return undefined;
    if (navigationReaderLockRef.current && !includeNavigationLock) {
      setReaderEntryID(navigationReaderLockRef.current);
      return undefined;
    }
    const node = transcriptRef.current;
    if (!node) return undefined;
    const position = readTranscriptPosition(
      node,
      transcriptWindowEndID,
      atBottomRef.current
    );
    if (!position) return undefined;
    readingPositionsRef.current.set(snapshot.selectedSessionID, position);
    setReaderEntryID(transcriptFocusEntryID(node) ?? position.entryID);
    return position;
  }, [activeView, snapshot.selectedSessionID, transcriptWindowEndID]);
  const scheduleReadingPositionCapture = useCallback(() => {
    if (readerFrameRef.current !== undefined) return;
    readerFrameRef.current = window.requestAnimationFrame(() => {
      readerFrameRef.current = undefined;
      readAndAdvanceRef.current();
    });
  }, []);
  const prepareTranscriptInteraction = useCallback((target: EventTarget | null) => {
    const node = transcriptRef.current;
    if (!node || !(target instanceof Element) ||
        !transcriptContentRef.current?.contains(target)) return;
    // Reading, selecting and activating transcript content take priority over
    // following new output. Capture before focus/click can change its geometry.
    atBottomRef.current = false;
    setAtBottom(false);
    scrollDirectionRef.current = 0;
    navigationReaderLockRef.current = "";
    pendingReadingRestoreRef.current = undefined;
    const element = target.closest("button, [role='button'], a, input, select, textarea") ?? target;
    interactionAnchorRef.current = {
      element, top: element.getBoundingClientRect().top - node.getBoundingClientRect().top
    };
    scrollTopRef.current = node.scrollTop;
    captureReadingPosition(true);
  }, [captureReadingPosition]);
  const restoreInteractionAnchor = useCallback(() => {
    const anchor = interactionAnchorRef.current;
    const node = transcriptRef.current;
    if (!anchor || !node) return false;
    if (!transcriptContentRef.current?.contains(anchor.element)) {
      interactionAnchorRef.current = undefined;
      return false;
    }
    node.scrollTop += anchor.element.getBoundingClientRect().top -
      node.getBoundingClientRect().top - anchor.top;
    scrollTopRef.current = node.scrollTop;
    return true;
  }, []);
  const loadTranscriptHistory = useCallback(async () => {
    if (historyLoadRef.current || !snapshot.historyMoreBefore ||
        !snapshot.selectedSessionID || snapshot.hydratingSessionID) return 0;
    const request = {};
    historyLoadRef.current = request;
    const anchor = pendingReadingRestoreRef.current ?? captureReadingPosition(true);
    const endID = anchor?.windowEndID ?? transcriptWindowEndID ?? visibleEntries.at(-1)?.id;
    if (anchor) {
      const saved = {...anchor, windowEndID: endID};
      pendingReadingRestoreRef.current = saved;
      readingPositionsRef.current.set(snapshot.selectedSessionID, saved);
    }
    setTranscriptWindowEndID(endID);
    setHistoryError("");
    setHistoryLoading(true);
    try {
      return await client.loadEarlierHistory();
    } catch (error) {
      if (historyLoadRef.current === request) {
        setHistoryError(error instanceof Error ? error.message : String(error));
      }
      return 0;
    } finally {
      if (historyLoadRef.current === request) {
        historyLoadRef.current = undefined;
        setHistoryLoading(false);
      }
    }
  }, [client, snapshot.historyMoreBefore, snapshot.selectedSessionID,
    snapshot.hydratingSessionID, transcriptWindowEndID, visibleEntries, captureReadingPosition]);

  readAndAdvanceRef.current = () => {
    if (!pendingReadingRestoreRef.current) captureReadingPosition();
    const node = transcriptRef.current;
    if (!node || node.clientHeight === 0 || activeView !== "chat" ||
        !selected || snapshot.hydratingSessionID || document.visibilityState === "hidden" ||
        navigationTarget || conversationNavigatorOpen || historyLoadRef.current) return;
    if (pendingReadingRestoreRef.current) {
      if (windowEndIndex < 0 && snapshot.historyMoreBefore && !historyError) void loadTranscriptHistory();
      return;
    }
    const direction = scrollDirectionRef.current;
    const shortContent = node.scrollHeight <= node.clientHeight;
    const earlier = node.scrollTop <= node.clientHeight && (direction < 0 || shortContent);
    const later = direction > 0 && transcriptEnd < entries.length &&
      node.scrollHeight - node.scrollTop - node.clientHeight <= node.clientHeight;
    if (!earlier && !later) return;
    if (earlier && transcriptStart === 0) {
      if (snapshot.historyMoreBefore && !historyError) void loadTranscriptHistory();
      return;
    }
    const visible = visibleTranscriptAnchors(node);
    const first = entries.findIndex((entry) => entry.id === visible[0]?.dataset.entryId);
    const last = entries.findIndex((entry) => entry.id === visible.at(-1)?.dataset.entryId);
    // Keep all currently visible entries inside the bounded, overlapping window.
    const nextEnd = earlier
      ? Math.max(transcriptEnd - transcriptPageStep, last >= 0 ? last + 1 : transcriptStart + 1)
      : Math.min(entries.length, transcriptEnd + transcriptPageStep,
        (first >= 0 ? first : transcriptEnd - transcriptPageOverlap) + transcriptPageSize);
    if (nextEnd === transcriptEnd || nextEnd <= 0) return;
    const endID = entries[nextEnd - 1]?.id;
    const anchor = captureReadingPosition(true);
    if (anchor) {
      const saved = {...anchor, windowEndID: endID, atBottom: false};
      pendingReadingRestoreRef.current = saved;
      readingPositionsRef.current.set(snapshot.selectedSessionID, saved);
    }
    atBottomRef.current = false;
    setAtBottom(false);
    setTranscriptWindowEndID(endID);
  };

  useEffect(() => {
    if (activeView !== "chat" || !selected || snapshot.hydratingSessionID) return;
    scheduleReadingPositionCapture();
    const node = transcriptRef.current;
    if (!node || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver(scheduleReadingPositionCapture, {root: node});
    if (historyTopRef.current) observer.observe(historyTopRef.current);
    if (historyBottomRef.current) observer.observe(historyBottomRef.current);
    return () => observer.disconnect();
  }, [activeView, snapshot.selectedSessionID, snapshot.hydratingSessionID,
    transcriptStart, transcriptEnd, snapshot.historyMoreBefore, historyLoading,
    scheduleReadingPositionCapture]);

  const switchConversationView = useCallback(
    (view: "chat" | "trajectory") => {
      if (view === activeView) return;
      if (activeView === "chat") captureReadingPosition(true);
      interactionAnchorRef.current = undefined;
      if (view === "chat") {
        pendingReadingRestoreRef.current = readingPositionsRef.current.get(
          snapshot.selectedSessionID
        );
      }
      setActiveView(view);
      if (view === "trajectory") {
        requestAnimationFrame(() => {
          if (transcriptRef.current) transcriptRef.current.scrollTop = 0;
        });
      }
    },
    [
      activeView,
      captureReadingPosition,
      snapshot.selectedSessionID
    ]
  );
  const jumpToNavigationItem = useCallback(
    (item: ConversationNavigationItem) => {
      const page = transcriptPageForEntry(
        entries,
        item.entryID,
        transcriptPageSize,
        transcriptPageStep
      );
      if (page === undefined) return;
      setNavigationTarget({
        entryID: item.entryID,
        path: item.path
      });
      setConversationNavigatorOpen(false);
      pendingReadingRestoreRef.current = undefined;
      interactionAnchorRef.current = undefined;
      scrollDirectionRef.current = 0;
      setTranscriptWindowEndID(entries[entries.length - page * transcriptPageStep - 1]?.id);
      setActiveView("chat");
      setNavigationHighlightID(item.entryID);
      setReaderEntryID(item.entryID);
      navigationReaderLockRef.current = item.entryID;
      atBottomRef.current = false;
      setAtBottom(false);
    },
    [entries]
  );
  const jumpToQuestion = useCallback(
    (item?: ConversationNavigationItem) => {
      if (item) jumpToNavigationItem(item);
    },
    [jumpToNavigationItem]
  );
  const openChatFromTrajectory = useCallback(
    (turnID: string, callID?: string, entryID?: string) => {
      const item = conversationNavigation.find(
        (candidate) => candidate.entryID === entryID && candidate.turnID === turnID
      ) ?? conversationNavigation.find(
        (candidate) =>
          candidate.kind === "tool" &&
          candidate.turnID === turnID &&
          candidate.callID === callID
      ) ?? conversationNavigation.find(
        (candidate) =>
          candidate.kind === "question" && candidate.turnID === turnID
      ) ?? conversationNavigation.find(
        (candidate) => candidate.turnID === turnID
      );
      if (item) jumpToNavigationItem(item);
    },
    [conversationNavigation, jumpToNavigationItem]
  );
  const openBackgroundActivity = useCallback(
    (target: BackgroundActivityTarget) => {
      window.focus();
      setActivityTarget(target);
      setTranscriptWindowEndID(undefined);
      setActiveView("chat");
      setConversationNavigatorOpen(false);
      void client.selectSession(target.sessionID).catch((error) => {
        setActivityTarget(undefined);
        reportLocalError(error);
      });
    },
    [client, reportLocalError]
  );
  const closeContext = useCallback(() => setContextOpen(false), []);
  const closeSettings = useCallback(() => {
    setSettingsOpen(false);
    setSettingsAddModel(false);
  }, []);
  const inspectTool = useCallback((callID: string) => {
    setInspectCallID(callID);
    switchConversationView("trajectory");
    void client.refreshTrace();
  }, [client, switchConversationView]);
  const attachmentBusy = composerAttachments.some(
    (attachment) => attachment.status === "processing"
  );
  const attachmentFailed = composerAttachments.some(
    (attachment) => attachment.status === "error"
  );
  const visibleContextResources = snapshot.contextResources.filter(
    (resource) =>
      resource.kind !== "attachment" &&
      !(resource.kind === "image" && !resource.path)
  );

  const attachFiles = (
    values: FileList | readonly File[],
    source: ComposerAttachmentSource
  ) => {
    const files = Array.from(values);
    if (
      files.length === 0 ||
      !snapshot.selectedSessionID ||
      snapshot.hydratingSessionID ||
      submitting
    ) {
      return;
    }
    setLocalError("");
    const processing = composerAttachments.filter(
      (attachment) => attachment.status === "processing"
    ).length;
    const available = Math.max(
      0,
      maxComposerAttachments - snapshot.contextResources.length - processing
    );
    const countAccepted = files.slice(0, available);
    let reservedBytes = composerAttachments
      .filter((attachment) => attachment.status !== "error")
      .reduce((total, attachment) => total + attachment.bytes, 0);
    const accepted = countAccepted.filter((file) => {
      if (reservedBytes + file.size > maxComposerAttachmentBytes) return false;
      reservedBytes += file.size;
      return true;
    });
    if (countAccepted.length < files.length) {
      setLocalError(`A prompt accepts at most ${maxComposerAttachments} context items`);
    } else if (accepted.length < countAccepted.length) {
      setLocalError("Attachments exceed the 5 MiB total prompt limit");
    }
    const generation = attachmentGenerationRef.current;
    const sessionID = snapshot.selectedSessionID;
    const pending = accepted.map((file) => ({
      file,
      attachment: {
        id: crypto.randomUUID(),
        name: file.name || "Pasted image",
        mediaType: file.type || "application/octet-stream",
        bytes: file.size,
        source,
        status: "processing" as const
      }
    }));
    setComposerAttachments((current) => [
      ...current,
      ...pending.map(({attachment}) => attachment)
    ]);
    const pipeline = import("./attachmentPipeline");
    for (const {file, attachment} of pending) {
      void pipeline.then(({prepareComposerAttachment}) =>
        prepareComposerAttachment(file)
      ).then((context) => {
        if (
          generation !== attachmentGenerationRef.current ||
          sessionID !== selectedSessionRef.current ||
          removedAttachmentIDs.current.has(attachment.id)
        ) {
          return;
        }
        if (client.getSnapshot().contextResources.some(
          (resource) => resource.digest === context.digest
        )) {
          throw new Error(`${context.label || attachment.name} is already attached`);
        }
        client.addAttachmentContext(context);
        setComposerAttachments((current) => current.map((value) =>
          value.id === attachment.id
            ? {
                ...value,
                name: context.label || value.name,
                mediaType: context.media_type || value.mediaType,
                digest: context.digest,
                status: "ready",
                error: undefined
              }
            : value
        ));
      }).catch((error) => {
        if (generation !== attachmentGenerationRef.current) return;
        setComposerAttachments((current) => current.map((value) =>
          value.id === attachment.id
            ? {
                ...value,
                status: "error",
                error: error instanceof Error ? error.message : String(error)
              }
            : value
        ));
      });
    }
  };

  const removeAttachment = (id: string) => {
    removedAttachmentIDs.current.add(id);
    const attachment = attachmentsRef.current.find((value) => value.id === id);
    if (attachment?.digest) client.removeAttachmentContext(attachment.digest);
    setComposerAttachments((current) => current.filter((value) => value.id !== id));
  };

  useEffect(() => {
    writePreference("ch.sidebar.collapsed", String(railCollapsed));
  }, [railCollapsed]);

  useEffect(() => {
    writePreference("ch.sidebar.width", String(railWidth));
  }, [railWidth]);

  useEffect(() => {
    writePreference("ch.session.isolation", newIsolation);
  }, [newIsolation]);

  useEffect(() => {
    if (
      !activityTarget ||
      snapshot.selectedSessionID !== activityTarget.sessionID ||
      snapshot.hydratingSessionID
    ) {
      return;
    }
    setActiveView("chat");
    const frame = window.requestAnimationFrame(() => {
      const pending = activityTarget.status === "awaiting_approval" ||
        activityTarget.status === "awaiting_input"
        ? document.querySelector<HTMLElement>(".pendingComposer")
        : undefined;
      const anchors = Array.from(
        document.querySelectorAll<HTMLElement>("[data-turn-id]")
      ).filter((node) => node.dataset.turnId === activityTarget.turnID);
      const target = pending ?? anchors.at(-1)?.firstElementChild ??
        transcriptRef.current;
      if (target instanceof HTMLElement) {
        target.scrollIntoView?.({block: "center"});
      }
      if (pending) {
        const focusTarget = activityTarget.status === "awaiting_approval"
          ? pending.querySelector<HTMLElement>(".approvalBody")
          : pending.querySelector<HTMLElement>(
            "textarea:not(:disabled), button:not(:disabled)"
          );
        focusTarget?.focus();
      }
      setActivityTarget(undefined);
    });
    return () => window.cancelAnimationFrame(frame);
  }, [
    activityTarget,
    snapshot.conversation.revision,
    snapshot.hydratingSessionID,
    snapshot.selectedSessionID
  ]);

  useLayoutEffect(() => {
    const saved = readingPositionsRef.current.get(snapshot.selectedSessionID);
    interactionAnchorRef.current = undefined;
    pendingReadingRestoreRef.current = saved;
    setTranscriptWindowEndID(saved?.windowEndID);
    historyLoadRef.current = undefined;
    setHistoryLoading(false);
    setHistoryError("");
    scrollDirectionRef.current = 0;
    scrollTopRef.current = 0;
    setNavigationTarget(undefined);
    setActiveView("chat");
    setInspectCallID("");
    setConversationNavigatorOpen(false);
    setReaderEntryID(saved?.entryID ?? "");
    setContextOpen(false);
    attachmentGenerationRef.current += 1;
    removedAttachmentIDs.current.clear();
    setComposerAttachments([]);
    setDraggingAttachment(false);
    setCommandMenuOpen(false);
    setCommandQuery("");
    setCommandMenuSource("button");
    atBottomRef.current = saved?.atBottom ?? true;
    setAtBottom(saved?.atBottom ?? true);
  }, [snapshot.selectedSessionID]);

  useEffect(() => {
    void client.start();
    return () => client.stop();
  }, [client]);

  useEffect(() => {
    if (snapshot.phase !== "ready") return;
    const refresh = () => {
      if (document.visibilityState === "hidden") return;
      void Promise.all([
        client.refreshWorkspaces(),
        client.refreshSessions(undefined, false)
      ]).catch(() => undefined);
    };
    document.addEventListener("visibilitychange", refresh);
    return () => document.removeEventListener("visibilitychange", refresh);
  }, [client, snapshot.phase]);

  useEffect(() => {
    setDraft(client.loadDraft(snapshot.selectedSessionID));
  }, [client, snapshot.selectedSessionID, snapshot.selectedWorkspaceID]);

  const updateDraft = (value: string) => {
    client.saveDraft(value, snapshot.selectedSessionID);
    setDraft(value);
  };

  useLayoutEffect(() => {
    const node = transcriptRef.current;
    if (!node || activeView !== "chat") return;
    const navigation = navigationTarget;
    if (navigation) {
      interactionAnchorRef.current = undefined;
      const anchor = transcriptAnchor(node, navigation.entryID);
      if (!anchor) return;
      setNavigationTarget(undefined);
      const target = navigation.path
        ? fileTarget(anchor, navigation.path) ?? anchorContent(anchor)
        : anchorContent(anchor);
      centerTranscriptTarget(node, target);
      scrollTopRef.current = node.scrollTop;
      atBottomRef.current = false;
      setAtBottom(false);
      setReaderEntryID(navigation.entryID);
      setNavigationHighlightID(navigation.entryID);
      if (navigationReaderLockTimerRef.current !== undefined) {
        window.clearTimeout(navigationReaderLockTimerRef.current);
      }
      navigationReaderLockTimerRef.current = window.setTimeout(() => {
        navigationReaderLockRef.current = "";
      }, 250);
      if (navigationHighlightTimerRef.current !== undefined) {
        window.clearTimeout(navigationHighlightTimerRef.current);
      }
      navigationHighlightTimerRef.current = window.setTimeout(
        () => setNavigationHighlightID(""),
        1_400
      );
      return;
    }
    if (restoreInteractionAnchor()) return;
    const saved = pendingReadingRestoreRef.current ??
      (!atBottomRef.current ? readingPositionsRef.current.get(snapshot.selectedSessionID) : undefined);
    if (saved && saved.windowEndID === transcriptWindowEndID &&
        transcriptAnchor(node, saved.entryID)) {
      pendingReadingRestoreRef.current = undefined;
      restoreTranscriptPosition(node, saved);
      scrollTopRef.current = node.scrollTop;
      atBottomRef.current = saved.atBottom;
      setAtBottom(saved.atBottom);
      setReaderEntryID(saved.entryID);
      return;
    }
    if (!snapshot.hydratingSessionID && !snapshot.historyMoreBefore &&
        windowEndIndex < 0) pendingReadingRestoreRef.current = undefined;
    if (atBottomRef.current && transcriptEnd === entries.length) {
      node.scrollTop = node.scrollHeight;
      scrollTopRef.current = node.scrollTop;
    }
  }, [
    activeView,
    navigationTarget,
    snapshot.conversation.revision,
    snapshot.hydratingSessionID,
    snapshot.historyMoreBefore,
    transcriptWindowEndID,
    transcriptEnd,
    entries.length,
    historyLoading,
    historyError,
    restoreInteractionAnchor
  ]);

  useEffect(() => {
    const content = transcriptContentRef.current;
    if (
      !content ||
      activeView !== "chat" ||
      typeof ResizeObserver === "undefined"
    ) {
      return;
    }
    const observer = new ResizeObserver(() => {
      const node = transcriptRef.current;
      if (!node) return;
      if (restoreInteractionAnchor()) {
        scheduleReadingPositionCapture();
        return;
      }
      if (atBottomRef.current && transcriptEnd === entries.length) {
        node.scrollTop = node.scrollHeight;
        scrollTopRef.current = node.scrollTop;
        scheduleReadingPositionCapture();
        return;
      }
      const saved = readingPositionsRef.current.get(snapshot.selectedSessionID);
      if (saved && saved.windowEndID === transcriptWindowEndID &&
          transcriptAnchor(node, saved.entryID)) {
        restoreTranscriptPosition(node, saved);
        scrollTopRef.current = node.scrollTop;
      }
      scheduleReadingPositionCapture();
    });
    observer.observe(content);
    return () => observer.disconnect();
  }, [activeView, snapshot.selectedSessionID, transcriptWindowEndID,
    transcriptEnd, entries.length, scheduleReadingPositionCapture, restoreInteractionAnchor]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const editable = isEditableElement(event.target);
      if (
        (event.metaKey || event.ctrlKey) &&
        event.key.toLocaleLowerCase() === "f" &&
        !editable &&
        !settingsOpen &&
        !contextOpen &&
        !commandMenuOpen &&
        selected &&
        entries.length > 0
      ) {
        event.preventDefault();
        setConversationNavigatorOpen(true);
        return;
      }
      if (editable || !event.altKey || event.metaKey || event.ctrlKey) return;
      if (event.key === "ArrowUp" && previousQuestion) {
        event.preventDefault();
        jumpToQuestion(previousQuestion);
      } else if (event.key === "ArrowDown" && nextQuestion) {
        event.preventDefault();
        jumpToQuestion(nextQuestion);
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [
    commandMenuOpen,
    contextOpen,
    entries.length,
    jumpToQuestion,
    nextQuestion,
    previousQuestion,
    selected,
    settingsOpen
  ]);

  useEffect(() => () => {
    historyLoadRef.current = undefined;
    if (readerFrameRef.current !== undefined) {
      window.cancelAnimationFrame(readerFrameRef.current);
    }
    if (navigationHighlightTimerRef.current !== undefined) {
      window.clearTimeout(navigationHighlightTimerRef.current);
    }
    if (navigationReaderLockTimerRef.current !== undefined) {
      window.clearTimeout(navigationReaderLockTimerRef.current);
    }
  }, []);

  useEffect(() => {
    const textarea = textareaRef.current;
    if (!textarea) return;
    textarea.style.height = "0";
    textarea.style.height = `${Math.min(textarea.scrollHeight, 336)}px`;
  }, [draft]);

  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return;
    const keepComposerVisible = () => {
      if (document.activeElement !== textareaRef.current) return;
      requestAnimationFrame(() => textareaRef.current?.scrollIntoView({
        block: "nearest"
      }));
    };
    viewport.addEventListener("resize", keepComposerVisible);
    viewport.addEventListener("scroll", keepComposerVisible);
    return () => {
      viewport.removeEventListener("resize", keepComposerVisible);
      viewport.removeEventListener("scroll", keepComposerVisible);
    };
  }, []);

  useEffect(() => {
    if (activeView !== "trajectory" || !activeTurn) return;
    void client.refreshTrace();
  }, [activeTurn, activeView, client, traceRefreshSequence]);

  useEffect(() => {
    const media = typeof window.matchMedia === "function"
      ? window.matchMedia("(prefers-color-scheme: dark)")
      : undefined;
    const apply = () => applyThemeMode(themeMode, media?.matches ?? false);
    apply();
    if (themeMode !== "system" || !media) return;
    media.addEventListener("change", apply);
    return () => media.removeEventListener("change", apply);
  }, [themeMode]);

  const submit = async (activeAction: "queue" | "steer" = "queue") => {
    const prompt = draft.trim();
    if (!prompt || submitting || attachmentBusy || attachmentFailed) return;
    const submittedSessionID = snapshot.selectedSessionID;
    const submittedTurnID = activeTurn;
    setSubmitting(true);
    setLocalError("");
    try {
      if (submittedTurnID && activeAction === "steer") {
        await client.steer(submittedTurnID, prompt);
      } else if (submittedTurnID) {
        await client.enqueue(submittedTurnID, prompt);
      } else if (resumableTurnID) {
        await client.recoverTurn(resumableTurnID, "continue", prompt);
      } else {
        await client.submitPrompt(prompt);
      }
      if (selectedSessionRef.current === submittedSessionID) {
        setComposerAttachments([]);
        removedAttachmentIDs.current.clear();
        if (draftRef.current.trim() === prompt) {
          updateDraft("");
        }
      }
    } catch (error) {
      setLocalError(error instanceof Error ? error.message : String(error));
    } finally {
      setSubmitting(false);
    }
  };

  const createSession = async (
    workspaceID = selectedWorkspace?.id,
    profilePatch?: Record<string, unknown>
  ) => {
    if (creatingSession) return;
    const workspace = snapshot.workspaces.find(
      (item) => item.id === workspaceID && item.ready
    );
    if (!workspace) {
      setWorkspaceDialogOpen(true);
      return;
    }
    setCreatingSession(true);
    setCreatingWorkspaceID(workspace.id);
    setLocalError("");
    try {
      if (workspace.id !== snapshot.selectedWorkspaceID) {
        await client.selectWorkspace(workspace.id);
      }
      await client.createSession(newIsolation, profilePatch);
    } catch (error) {
      reportLocalError(error);
    } finally {
      setCreatingSession(false);
      setCreatingWorkspaceID("");
    }
  };

  const removeWorkspace = async (workspaceID: string) => {
    const workspace = snapshot.workspaces.find((item) => item.id === workspaceID);
    if (!workspace?.removable || workspaceRemoving) return;
    setWorkspaceRemoving(true);
    try {
      await client.removeWorkspace(workspaceID);
      setWorkspaceRemovalID("");
    } catch (error) {
      reportLocalError(error);
    } finally {
      setWorkspaceRemoving(false);
    }
  };

  const composerCommands: ComposerCommand[] = [
    {
      id: "attach",
      label: "attach",
      description: "Attach local text files or images",
      argumentHint: "file",
      icon: Paperclip,
      run: () => attachmentInputRef.current?.click()
    },
    {
      id: "context",
      label: "context",
      description: "Browse files, symbols, diagnostics, and diffs",
      argumentHint: "file, symbol, or diff",
      icon: FileCode2,
      run: () => setContextOpen(true)
    },
    {
      id: "compact",
      label: "compact",
      description: "Compact older conversation history",
      icon: Braces,
      disabled: Boolean(activeTurn) || !selected?.latest_turn_id,
      run: async () => {
        try {
          await client.compactThread();
        } catch (error) {
          reportLocalError(error);
        }
      }
    },
    {
      id: "plan",
      label: "plan",
      description: "Analyze and propose a plan before implementation",
      icon: TextSelect,
      active: snapshot.profile?.profile.mode === "plan",
      disabled: !profileMutable(snapshot, "mode") || Boolean(profilePending),
      run: () => updateComposerProfile({mode: "plan"}, "Updating mode")
    },
    {
      id: "act",
      label: "act",
      description: "Execute the requested coding task",
      icon: Wrench,
      active: snapshot.profile?.profile.mode === "act",
      disabled: !profileMutable(snapshot, "mode") || Boolean(profilePending),
      run: () => updateComposerProfile({mode: "act"}, "Updating mode")
    },
    {
      id: "suggest",
      label: "suggest",
      description: "Ask before consequential tool actions",
      icon: AlertTriangle,
      active: snapshot.profile?.profile.approval_posture === "suggest",
      disabled: !profileMutable(snapshot, "approval_posture") ||
        Boolean(profilePending),
      run: () => updateComposerProfile({
        approval_posture: "suggest"
      }, "Updating approval")
    },
    {
      id: "auto",
      label: "auto",
      description: "Approve actions allowed by the current policy",
      icon: Check,
      active: snapshot.profile?.profile.approval_posture === "auto",
      disabled: !profileMutable(snapshot, "approval_posture") ||
        Boolean(profilePending),
      run: () => updateComposerProfile({
        approval_posture: "auto"
      }, "Updating approval")
    }
  ];

  const runSessionAction = async (
    session: SessionSummary,
    action: () => Promise<void>
  ) => {
    setSessionAction({
      sessionID: session.session_id,
      pending: true,
      error: ""
    });
    try {
      await action();
      setSessionAction(undefined);
    } catch (error) {
      setSessionAction({
        sessionID: session.session_id,
        pending: false,
        error: error instanceof Error ? error.message : String(error)
      });
    }
  };

  const deleteSession = (session: SessionSummary) => {
    const hasUnfinishedWork =
      sessionIsBusy(session) || session.isolation === "worktree";
    const prompt = hasUnfinishedWork
      ? `Delete "${session.title}" and permanently discard its unfinished work?`
      : `Delete "${session.title}" and permanently discard its unfinished workspace draft if one exists?`;
    if (!window.confirm(prompt)) return;
    void runSessionAction(session, () => client.deleteSession(
      session.session_id,
      session.revision,
      true
    ));
  };

  if (snapshot.phase === "setup" && snapshot.setupCatalog) {
    return (
      <FirstRunSetup
        catalog={snapshot.setupCatalog}
        workspaceRoot={snapshot.workspaceRoot}
        client={client}
      />
    );
  }
  if (snapshot.phase === "booting") {
    return <BootState title="Starting QCode" detail={snapshot.workspaceRoot} />;
  }
  if (snapshot.phase === "failed") {
    return (
      <BootState
        title="Runtime unavailable"
        detail={snapshot.problem?.message ?? "The Runtime could not start."}
        failed
      />
    );
  }

  return (
    <div
      className="app"
      data-rail-collapsed={!compactViewport && railCollapsed || undefined}
      data-mobile-rail={mobileRailOpen || undefined}
      data-motion={motionEnabled ? undefined : "none"}
      style={{
        "--ch-rail-width": `${railWidth}px`
      } as React.CSSProperties}
    >
      <Suspense fallback={null}>
        <BackgroundActivityMonitor
          sessions={snapshot.sessions}
          selectedSessionID={snapshot.selectedSessionID}
          onOpen={openBackgroundActivity}
        />
      </Suspense>
      <Presence open={compactViewport && mobileRailOpen} kind="fade" backdrop>
        <button className="railBackdrop" data-modal-backdrop data-motion-backdrop aria-label="Close session drawer"
          tabIndex={-1} onClick={() => setMobileRailOpen(false)} />
      </Presence>
      <aside ref={railRef} id="session-rail" className="sessionRail" aria-label="Sessions"
        {...(compactViewport && !mobileRailOpen ? {inert: "", "aria-hidden": true} : {})}
        role={compactViewport && mobileRailOpen ? "dialog" : undefined}
        aria-modal={compactViewport && mobileRailOpen || undefined}>
        <div className="brandRow">
          <button
            className="railToggle"
            aria-label={railCollapsed ? "Expand sidebar" : "Collapse sidebar"}
            title={railCollapsed ? "Expand sidebar" : "Collapse sidebar"}
            onClick={() => compactViewport ? setMobileRailOpen(false) : setRailCollapsed((value) => !value)}
          >
            <span className="railLogo"><CapybaraMark /></span>
            <span className="railToggleIcon">
              {railCollapsed
                ? <PanelLeftOpen size={17} />
                : <PanelLeftClose size={17} />}
            </span>
          </button>
          <span className="brandName">QCode</span>
        </div>
        <div className="sessionSectionHeader">
          <span className="sessionSectionTitle">Workspaces</span>
          <div className="sessionSectionActions">
            <button
              type="button"
              className="addWorkspaceButton"
              onClick={() => setWorkspaceDialogOpen(true)}
            >
              <FolderPlus size={14} />
              <span>Add workspace</span>
            </button>
            <IconButton
              label="Search sessions"
              icon={<Search size={15} />}
              onClick={() => setSessionSearchOpen((value) => !value)}
            />
          </div>
        </div>
        {(sessionSearchOpen || query) && (
          <label className="searchBox">
            <Search size={14} aria-hidden="true" />
            <span className="srOnly">Search sessions</span>
            <input
              autoFocus
              value={query}
              placeholder="Search sessions"
              onChange={(event) => {
                const value = event.target.value;
                void client.refreshSessions(value);
              }}
              onKeyDown={(event) => {
                if (event.key !== "Escape") return;
                setSessionSearchOpen(false);
                void client.refreshSessions("");
              }}
            />
            <button
              className="clearSearch"
              aria-label="Close session search"
              onClick={() => {
                setSessionSearchOpen(false);
                void client.refreshSessions("");
              }}
            >
              <X size={14} />
            </button>
          </label>
        )}
          {(sessionSearchOpen || query) && (
            <button
              className="workspaceRow archiveFilter"
              aria-pressed={snapshot.includeArchived}
              onClick={() => void client.setArchivedVisible(
                !snapshot.includeArchived
              ).catch(reportLocalError)}
            >
              <Archive size={13} />
              {snapshot.includeArchived ? "Hide archived" : "Show archived"}
            </button>
          )}
        <div className="sessionList" aria-label="Workspace sessions">
            {snapshot.workspaces.map((workspace) => {
              const expanded = Boolean(query) ||
                !collapsedWorkspaceIDs.has(workspace.id);
              const workspaceSessions = (query.trim()
                ? snapshot.sessionSearchResults : snapshot.sessions).filter(
                (session) => session.workspace_root === workspace.root
              );
              return (
                <div className="workspaceGroup" key={workspace.id}>
                  <div
                    className="workspaceHeader"
                    data-active={
                      workspace.id === snapshot.selectedWorkspaceID || undefined
                    }
                    data-error={Boolean(workspace.problem) || undefined}
                  >
                    <button
                      className="workspaceRow"
                      aria-expanded={expanded}
                      onClick={() => {
                        if (workspace.id !== snapshot.selectedWorkspaceID) {
                          setCollapsedWorkspaceIDs((current) => {
                            const next = new Set(current);
                            next.delete(workspace.id);
                            return next;
                          });
                          void client.selectWorkspace(workspace.id).catch(reportLocalError);
                          return;
                        }
                        setCollapsedWorkspaceIDs((current) => {
                          const next = new Set(current);
                          if (next.has(workspace.id)) next.delete(workspace.id);
                          else next.add(workspace.id);
                          return next;
                        });
                      }}
                    >
                      <DisclosureLeading
                        open={expanded}
                        icon={<FolderOpen size={16} />}
                      />
                      <span className="sessionTitle">{workspace.label}</span>
                      {!workspace.git?.repository && (
                        <small>
                          {workspace.problem
                            ? "!"
                            : workspace.ready
                              ? workspaceSessions.length
                              : "…"}
                        </small>
                      )}
                    </button>
                    <span className="workspaceCreateAction">
                      <IconButton
                        label={`New session in ${workspace.label}`}
                        icon={creatingSession &&
                          workspace.id === creatingWorkspaceID
                          ? <LoaderCircle className="spin" size={14} />
                          : <MessageSquarePlus size={15} />}
                        disabled={!workspace.ready || creatingSession}
                        onClick={() => void createSession(workspace.id)}
                      />
                    </span>
                    <WorkspaceRemovalButton
                      id={workspace.id}
                      label={workspace.label}
                      removable={workspace.removable}
                      onRemove={setWorkspaceRemovalID}
                    />
                  </div>
                  {expanded && (
                    <div className="sessionGroup">
                      {workspaceSessions.map((session) => (
                        <SessionRow
                          key={session.session_id}
                          session={session}
                          active={session.session_id === snapshot.selectedSessionID}
                          onClick={() => {
                            captureReadingPosition(true);
                            setMobileRailOpen(false);
                            setLocalError("");
                            void client.selectSession(session.session_id)
                              .catch(reportLocalError);
                          }}
                          onRename={() => {
                            const title = window.prompt(
                              "Rename session",
                              session.title
                            )?.trim();
                            if (title && (title !== session.title ||
                              (session.title_source && session.title_source !== "manual"))) {
                              void runSessionAction(session, () => client.updateSession(
                                session.session_id, session.revision, {title}
                              ));
                            }
                          }}
                          onPin={() => void runSessionAction(
                            session,
                            () => client.updateSession(
                              session.session_id,
                              session.revision,
                              {pinned: !session.pinned}
                            )
                          )}
                          onArchive={() => {
                            if (
                              session.archived ||
                              window.confirm(`Archive "${session.title}"?`)
                            ) {
                              void runSessionAction(session, () => client.updateSession(
                                session.session_id,
                                session.revision,
                                {archived: !session.archived}
                              ));
                            }
                          }}
                          onDelete={() => deleteSession(session)}
                          actionPending={
                            sessionAction?.sessionID === session.session_id &&
                            sessionAction.pending
                          }
                          actionError={
                            sessionAction?.sessionID === session.session_id
                              ? sessionAction.error
                              : ""
                          }
                        />
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
        </div>
        <div className="railFooter">
          <span className="connectionState" data-online={snapshot.socketConnected || undefined}>
            <span className="statusDot" />
            {snapshot.socketConnected ? "Connected" : snapshot.phase}
          </span>
          <span className="mobileWorkspaceAction">
            <IconButton
              label="Manage workspaces"
              icon={<FolderOpen size={16} />}
              onClick={() => setWorkspaceDialogOpen(true)}
            />
          </span>
          <IconButton
            label="Settings"
            icon={<Settings2 size={16} />}
            onClick={() => {
              setSettingsSection("general");
              setSettingsOpen((value) => !value);
            }}
          />
        </div>
        {!railCollapsed && (
          <ResizeHandle
            label="Resize sidebar"
            value={railWidth}
            minimum={experience.layout.sidebarMinimum}
            maximum={experience.layout.sidebarMaximum}
            onDelta={(delta) => setRailWidth((width) =>
              clamp(
                width + delta,
                experience.layout.sidebarMinimum,
                experience.layout.sidebarMaximum
              )
            )}
          />
        )}
      </aside>

      <main className="conversation" data-empty={blankSession || undefined}>
        <header
          className="conversationHeader"
        >
          <div className="mobileRailToggle">
            <IconButton label="Open session drawer" icon={<Menu size={18} />}
              expanded={mobileRailOpen} controls="session-rail"
              onClick={() => setMobileRailOpen(true)} />
          </div>
          <div className="conversationIdentity">
            <div>
              <h1>{selected?.title ?? "New Chat"}</h1>
              <p>{selected?.workspace_label || snapshot.workspaceRoot}</p>
            </div>
            {selected && entries.length > 0 && (
              <nav className="viewTabs" aria-label="Conversation views">
                <button
                  aria-current={activeView === "chat" ? "page" : undefined}
                  onClick={() => switchConversationView("chat")}
                >
                  Chat
                </button>
                <button
                  aria-current={activeView === "trajectory" ? "page" : undefined}
                  onClick={() => {
                    switchConversationView("trajectory");
                    void client.refreshTrace();
                  }}
                >
                  Trajectory
                </button>
              </nav>
            )}
          </div>
          <div className="headerActions">
            {snapshot.hydratingSessionID ? (
              <span className="workingLabel">Loading</span>
            ) : profilePending ? (
              <span className="workingLabel">{profilePending}</span>
            ) : activeTurn ? (
              <span className="workingLabel">Working</span>
            ) : selected?.status === "interrupted" ? (
              <span className="workingLabel" data-paused role="status">
                <CirclePause size={13} />
                Paused
              </span>
            ) : null}
            {selected && currentQuestion.total > 0 && (
              <div
                className="conversationNavigationControls"
                aria-label="Question navigation"
              >
                <IconButton
                  label="Previous user question"
                  disabled={!previousQuestion}
                  icon={<ChevronUp size={16} />}
                  onClick={() => jumpToQuestion(previousQuestion)}
                />
                <button
                  type="button"
                  className="conversationNavigationPosition"
                  aria-label="Search conversation"
                  title="Search conversation"
                  data-current-entry={readerEntryID || undefined}
                  onClick={() => setConversationNavigatorOpen(true)}
                >
                  <Search size={14} />
                  <span>
                    {currentQuestion.index + 1}/{currentQuestion.total}
                  </span>
                </button>
                <IconButton
                  label="Next user question"
                  disabled={!nextQuestion}
                  icon={<ChevronDown size={16} />}
                  onClick={() => jumpToQuestion(nextQuestion)}
                />
              </div>
            )}
            {selectedWorkspace && (
                <IconButton
                  label="Git tools"
                  icon={<GitBranch size={17} />}
                  expanded={gitOpen && activeView === "chat"}
                  controls="git-tools"
                  onClick={() => {
                    if (activeView === "trajectory") {
                      switchConversationView("chat");
                      setGitOpen(true);
                    } else setGitOpen((value) => !value);
                  }}
                />
            )}
          </div>
        </header>

        <div
          className="conversationScrollport"
          ref={transcriptRef}
          data-conversation-scroll
          aria-busy={Boolean(snapshot.hydratingSessionID)}
          data-view={activeView}
          onScroll={(event) => {
            const node = event.currentTarget;
            if (activeView !== "chat") return;
            const delta = node.scrollTop - scrollTopRef.current;
            scrollTopRef.current = node.scrollTop;
            // Layout/resize restoration already records its final scrollTop.
            // A delayed event from that write must not change the user's intent
            // using a newer scrollHeight or trigger another history window shift.
            if (delta === 0) return;
            interactionAnchorRef.current = undefined;
            navigationReaderLockRef.current = "";
            scrollDirectionRef.current = delta < 0 ? -1 : 1;
            const next = transcriptEnd === entries.length &&
              node.scrollHeight - node.scrollTop - node.clientHeight <=
              experience.scrolling.followThreshold;
            atBottomRef.current = next;
            setAtBottom(next);
            const windowEndID = transcriptWindowEndID ??
              (!next ? visibleEntries.at(-1)?.id : undefined);
            // A queued stream render can run before the next animation frame.
            // Commit the user's position now; only navigation/history work is deferred.
            const position = readTranscriptPosition(node, windowEndID, next);
            if (position) {
              readingPositionsRef.current.set(snapshot.selectedSessionID, position);
              if (historyLoadRef.current) pendingReadingRestoreRef.current = position;
            }
            if (!next && !transcriptWindowEndID) {
              setTranscriptWindowEndID(windowEndID);
            }
            scheduleReadingPositionCapture();
          }}
        >
          {activeView === "trajectory" && selected ? (
            <Suspense fallback={<Skeleton label="Loading trajectory" />}>
              <Trajectory
                events={snapshot.events}
                trace={snapshot.trace}
                tracePhase={snapshot.tracePhase}
                traceProblem={snapshot.traceProblem}
                hasEarlier={snapshot.historyMoreBefore}
                inspectCallID={inspectCallID}
                onInspectConsumed={() => setInspectCallID("")}
                onLoadEarlier={() => client.loadEarlierHistory()}
                onRetryTrace={() => client.refreshTrace()}
                onOpenChat={openChatFromTrajectory}
              />
            </Suspense>
          ) : <div
            className="transcript"
            ref={transcriptContentRef}
            aria-live="polite"
            onPointerDownCapture={(event) => prepareTranscriptInteraction(event.target)}
            onFocusCapture={(event) => {
              if (!navigationTarget && !navigationReaderLockRef.current) {
                prepareTranscriptInteraction(event.target);
              }
            }}
            onKeyDownCapture={(event) => {
              if (event.key === "Enter" || event.key === " ") {
                prepareTranscriptInteraction(event.target);
              }
            }}
            onClickCapture={(event) => prepareTranscriptInteraction(event.target)}
          >
            {snapshot.problem && (
              <div className="inlineProblem">
                <AlertTriangle size={17} />
                <span>{snapshot.problem.message}</span>
                <IconButton
                  label="Reconnect"
                  icon={<RefreshCw size={15} />}
                  onClick={() => void client.start()}
                />
              </div>
            )}
            {localError && (
              <div className="inlineProblem" role="alert">
                <AlertTriangle size={17} />
                <span>{localError}</span>
                <IconButton
                  label="Dismiss error"
                  icon={<X size={14} />}
                  onClick={() => setLocalError("")}
                />
              </div>
            )}
            {(transcriptStart > 0 || snapshot.historyMoreBefore) && (
              <div ref={historyTopRef} className="transcriptBoundary" data-history-edge="earlier" aria-hidden="true" />
            )}
            {historyLoading && (
              <div className="transcriptHistoryStatus" role="status" aria-label="Loading earlier messages">
                <LoaderCircle className="spin" size={16} aria-hidden="true" />
              </div>
            )}
            {historyError && (
              <div className="transcriptHistoryStatus" role="alert">
                <span>{historyError}</span>
                <IconButton label="Retry loading history" icon={<RefreshCw size={16} />}
                  onClick={() => void loadTranscriptHistory()} />
              </div>
            )}
            {!selected ? (
              <EmptySessionSetup
                creating={creatingSession}
                workspaceReady={Boolean(selectedWorkspace)}
                onCreate={() => void createSession(selectedWorkspace?.id)}
                onChooseWorkspace={() => setWorkspaceDialogOpen(true)}
              />
            ) : entries.length === 0 && snapshot.hydratingSessionID ? (
              <Skeleton label="Loading conversation" />
            ) : entries.length === 0 ? (
              <div className="emptyConversation">
                <QCodeWordmark hero />
                <p>{snapshot.workspaceRoot}</p>
              </div>
            ) : (
              groupTranscriptTurns(visibleEntries).map((turn) => (
                <TurnTranscript
                  key={turn.id}
                  entries={turn.entries}
                  terminalKind={terminalTurns.get(turn.turnID)}
                  preserveReading={!atBottom}
                  revealEntryID={navigationTarget?.entryID}
                  client={client}
                  onError={reportLocalError}
                  onInspect={inspectTool}
                  checkpoints={snapshot.checkpoints}
                  recoveryTurnID={resumableTurnID}
                  canWithdraw={selected?.latest_turn_id === turn.turnID && !selected.latest_turn_withdrawn}
                  withdrawalCommitted={selected?.latest_turn_id === turn.turnID && selected.latest_turn_withdrawn}
                  chrome={turnChrome.get(turn.turnID)}
                  messageFeedback={snapshot.messageFeedback}
                  selectedSessionID={snapshot.selectedSessionID}
                  navigationHighlightID={navigationHighlightID}
                />
              ))
            )}
            {transcriptEnd < entries.length && (
              <div ref={historyBottomRef} className="transcriptBoundary" data-history-edge="newer" aria-hidden="true" />
            )}
            {activeTurn && transcriptEnd === entries.length &&
              <TurnStatus events={presentationEvents} turnID={activeTurn}
                status={snapshot.conversation.activeStatus} />}
          </div>}

          {selected && <div className="composerSeat" data-composer-seat>
          <Presence open={activeView === "chat" && !atBottom && entries.length > 0} kind="fade">
            <div className="backToBottom" data-motion-surface>
              <IconButton
                label="Back to bottom"
                icon={<ArrowDown size={17} />}
                onClick={() => {
                  const node = transcriptRef.current;
                  if (!node) return;
                  pendingReadingRestoreRef.current = undefined;
                  interactionAnchorRef.current = undefined;
                  scrollDirectionRef.current = 0;
                  setTranscriptWindowEndID(undefined);
                  setNavigationTarget(undefined);
                  navigationReaderLockRef.current = "";
                  if (transcriptEnd === entries.length) {
                    node.scrollTo({top: node.scrollHeight, behavior: motionEnabled ? "smooth" : "auto"});
                  }
                  atBottomRef.current = true;
                  setAtBottom(true);
                  readingPositionsRef.current.delete(snapshot.selectedSessionID);
                  setReaderEntryID(
                    conversationNavigation
                      .filter((item) => item.kind === "question")
                      .at(-1)?.entryID ?? ""
                  );
                }}
              />
            </div>
          </Presence>

            {visibleContextResources.length > 0 && (
              <div className="contextTray" aria-label="Prompt context">
                {visibleContextResources.map((resource) => (
                  <span
                    className="contextItem"
                    key={`${resource.kind}:${resource.path ?? ""}:${resource.label ?? ""}:${resource.symbol?.name ?? ""}:${resource.digest}`}
                  >
                    <FileCode2 size={13} />
                    <span>{contextResourceLabel(resource)}</span>
                    <IconButton
                      label={`Remove ${contextResourceLabel(resource)} from prompt context`}
                      icon={<X size={12} />}
                      onClick={() => client.removeContext(
                        resource.kind,
                        resource.path,
                        resource.label,
                        resource.symbol?.name
                      )}
                    />
                  </span>
                ))}
              </div>
            )}
            {(snapshot.plan || snapshot.agents.length > 0) && (
              <Suspense fallback={null}>
                <SessionProgress
                  plan={snapshot.plan}
                  agents={snapshot.agents}
                  activeTurnID={activeTurn}
                  latestTurnID={snapshot.conversation.latestTurnID}
                  onOpenTrajectory={() => {
                    switchConversationView("trajectory");
                    void client.refreshTrace();
                  }}
                />
              </Suspense>
            )}
            {snapshot.queuedTurns.length > 0 && (
              <Suspense fallback={null}>
                <TurnQueue
                  items={snapshot.queuedTurns}
                  activeTurnID={activeTurn}
                  onUpdate={(queueID, prompt) =>
                    client.updateQueuedTurn(queueID, prompt).then(() => undefined)}
                  onRemove={(queueID) =>
                    client.removeQueuedTurn(queueID).then(() => undefined)}
                  onPromote={(queueID, turnID) =>
                    client.promoteQueuedTurn(queueID, turnID).then(() => undefined)}
                  onError={reportLocalError}
                />
              </Suspense>
            )}
            {pendingApproval ? (
              <ApprovalComposer
                key={pendingApprovalKey}
                event={pendingApproval}
                client={client}
                stopping={cancelingTurnID === (activeTurn || pendingApproval.turn_id)}
                onStop={() => requestCancel(activeTurn || pendingApproval.turn_id)}
              />
            ) : pendingInput ? (
              <InputComposer
                key={pendingInputKey}
                event={pendingInput}
                client={client}
                stopping={cancelingTurnID === (activeTurn || pendingInput.turn_id)}
                onStop={() => requestCancel(activeTurn || pendingInput.turn_id)}
              />
            ) : (
              <div
                className="composer"
                data-dragging={draggingAttachment || undefined}
                onDragEnter={(event) => {
                  if (!event.dataTransfer.types.includes("Files")) return;
                  event.preventDefault();
                  setDraggingAttachment(true);
                }}
                onDragOver={(event) => {
                  if (!event.dataTransfer.types.includes("Files")) return;
                  event.preventDefault();
                  event.dataTransfer.dropEffect = "copy";
                }}
                onDragLeave={(event) => {
                  if (event.currentTarget.contains(event.relatedTarget as Node)) return;
                  setDraggingAttachment(false);
                }}
                onDrop={(event) => {
                  event.preventDefault();
                  setDraggingAttachment(false);
                  attachFiles(event.dataTransfer.files, "drop");
                }}
              >
                <input
                  ref={attachmentInputRef}
                  className="srOnly"
                  type="file"
                  multiple
                  accept={composerAttachmentAccept}
                  aria-label="Attach files"
                  disabled={Boolean(snapshot.hydratingSessionID) || submitting}
                  onChange={(event) => {
                    if (event.target.files) {
                      attachFiles(event.target.files, "picker");
                    }
                    event.target.value = "";
                  }}
                />
                {composerAttachments.length > 0 && (
                  <Suspense fallback={null}>
                    <ComposerAttachments
                      attachments={composerAttachments}
                      onRemove={removeAttachment}
                    />
                  </Suspense>
                )}
                <div className="composerInputRow">
                  <textarea
                    ref={textareaRef}
                    value={draft}
                    rows={1}
                    placeholder="Ask QCode"
                    enterKeyHint="send"
                    disabled={Boolean(snapshot.hydratingSessionID) || submitting}
                    onChange={(event) => {
                      const value = event.target.value;
                      updateDraft(value);
                      const slashQuery = composerSlashQuery(value);
                      if (slashQuery !== undefined) {
                        setCommandMenuSource("slash");
                        setCommandQuery(slashQuery);
                        setCommandMenuOpen(true);
                      } else if (commandMenuSource === "slash") {
                        setCommandMenuOpen(false);
                        setCommandQuery("");
                      }
                    }}
                    onCompositionStart={() => {
                      composingRef.current = true;
                    }}
                    onCompositionEnd={() => {
                      composingRef.current = false;
                    }}
                    onPaste={(event) => {
                      const files = Array.from(event.clipboardData.files);
                      if (files.length === 0) return;
                      event.preventDefault();
                      attachFiles(files, "paste");
                    }}
                    onKeyDown={(event) => {
                      if (event.nativeEvent.isComposing || composingRef.current) return;
                      if (
                        commandMenuOpen &&
                        commandMenuSource === "slash" &&
                        composerSlashQuery(draft) !== undefined
                      ) {
                        if (event.key === "Enter") event.preventDefault();
                        return;
                      }
                      if (event.key === "Enter" && !event.shiftKey) {
                        event.preventDefault();
                        void submit(
                          activeTurn && (event.metaKey || event.ctrlKey)
                            ? "steer"
                            : "queue"
                        );
                      }
                    }}
                  />
                  <ContextMeter
                    attribution={contextAttribution}
                    fallbackUsed={numberValue(
                      isObject(latestReceipt?.data.context_budget)
                        ? latestReceipt.data.context_budget.active_tokens
                        : 0
                    )}
                    capacity={numberValue(
                      isObject(latestReceipt?.data.context_budget)
                        ? latestReceipt.data.context_budget.max_context_tokens
                        : 0
                    ) || selectedModelEntry?.capabilities.context_window}
                  />
                  <div className="composerActions">
                    {activeTurn && (
                      <IconButton
                        label="Stop turn"
                        danger
                        disabled={cancelingTurnID === activeTurn}
                        icon={cancelingTurnID === activeTurn
                          ? <LoaderCircle className="spin" size={19} />
                          : <CircleStop size={19} />}
                        onClick={() => requestCancel(activeTurn)}
                      />
                    )}
                    {(!activeTurn || Boolean(draft.trim())) && (
                      <>
                        {activeTurn &&
                          snapshot.contextResources.length === 0 &&
                          composerAttachments.length === 0 && (
                          <IconButton
                            label="Steer current turn"
                            disabled={submitting}
                            icon={<Zap size={18} />}
                            onClick={() => void submit("steer")}
                          />
                        )}
                        <IconButton
                          label={activeTurn
                            ? "Queue next"
                            : resumableTurnID
                              ? "Continue"
                              : "Send"}
                          primary
                          disabled={
                            Boolean(snapshot.hydratingSessionID) ||
                            !draft.trim() ||
                            submitting ||
                            attachmentBusy ||
                            attachmentFailed ||
                            Boolean(resumableTurnID && composerAttachments.length)
                          }
                          icon={submitting
                            ? <LoaderCircle className="spin" size={19} />
                            : activeTurn
                              ? <ListPlus size={19} />
                              : <Send size={19} />}
                          onClick={() => void submit("queue")}
                        />
                      </>
                    )}
                  </div>
                </div>
                <div className="composerControls">
                  <div>
                    <IconButton
                      label="Attach files"
                      icon={<Paperclip size={15} />}
                      disabled={
                        Boolean(snapshot.hydratingSessionID) ||
                        submitting ||
                        Boolean(resumableTurnID) ||
                        snapshot.contextResources.length >= maxComposerAttachments
                      }
                      onClick={() => attachmentInputRef.current?.click()}
                    />
                    <Suspense fallback={null}>
                      <ComposerCommandMenu
                        commands={composerCommands}
                        disabled={Boolean(snapshot.hydratingSessionID) || submitting}
                        open={commandMenuOpen}
                        query={commandQuery}
                        onOpenChange={(open) => {
                          setCommandMenuOpen(open);
                          if (open) {
                            setCommandMenuSource("button");
                            setCommandQuery("");
                          } else if (commandMenuSource === "slash") {
                            updateDraft("");
                            setCommandQuery("");
                            setCommandMenuSource("button");
                          }
                        }}
                        onQueryChange={setCommandQuery}
                        onSelect={() => {
                          setCommandQuery("");
                          if (commandMenuSource === "slash") {
                            updateDraft("");
                          }
                          setCommandMenuSource("button");
                        }}
                        onRequestComposerFocus={
                          commandMenuSource === "slash"
                            ? () => textareaRef.current?.focus()
                            : undefined
                        }
                      />
                    </Suspense>
                    <CompactSelect
                      label="Mode"
                      value={snapshot.profile?.profile.mode ?? "act"}
                      values={["plan", "act", "operate"]}
                      disabled={!profileMutable(snapshot, "mode") ||
                        Boolean(profilePending)}
                      onChange={(value) => void updateComposerProfile(
                        {mode: value},
                        "Updating mode"
                      )}
                    />
                    <CompactSelect
                      label="Approval"
                      value={snapshot.profile?.profile.approval_posture ?? "auto"}
                      values={["suggest", "auto", "never"]}
                      disabled={!profileMutable(snapshot, "approval_posture") ||
                        Boolean(profilePending)}
                      onChange={(value) => void updateComposerProfile(
                        {approval_posture: value},
                        "Updating approval"
                      )}
                    />
                  </div>
                  <div>
                    <CompactCatalogSelect
                      label="Model"
                      value={selectedModel}
                      options={[
                        ...modelOptions,
                        {value: "__configure__", label: "New model..."}
                      ]}
                      disabled={Boolean(profilePending)}
                      onChange={(model) => {
                        if (model === "__configure__") {
                          setSettingsSection("models");
                          setSettingsAddModel(true);
                          setSettingsOpen(true);
                          return;
                        }
                        if (!profileMutable(snapshot, "model")) return;
                        const target = snapshot.models.find(
                          (entry) =>
                            entry.provider === selectedProvider &&
                            entry.id === model
                        );
                        void updateComposerProfile({
                          model,
                          reasoning_effort:
                            target?.capabilities.default_reasoning_effort ?? ""
                        }, "Updating model");
                      }}
                    />
                    {advertisedReasoningValues.length > 0 && (
                      <ReasoningMenu
                        value={snapshot.profile?.profile.reasoning_effort ?? ""}
                        defaultValue={
                          selectedModelEntry?.capabilities.default_reasoning_effort
                        }
                        values={reasoningValues}
                        disabled={!profileMutable(snapshot, "reasoning_effort") ||
                          Boolean(profilePending)}
                        onChange={(value) => void updateComposerProfile({
                          reasoning_effort: value
                        }, "Updating reasoning")}
                      />
                    )}
                  </div>
                </div>
              </div>
            )}
            <ComposerStats
              receipt={latestReceipt?.data}
              usage={snapshot.usage}
              toolCalls={entries.filter((entry) => entry.kind === "tool").length}
            />
          </div>}
        </div>
      </main>

      <Presence open={gitOpen && activeView === "chat" && Boolean(selectedWorkspace)} kind="dialog">
        {selectedWorkspace && <Suspense fallback={null}>
          <GitTools
            key={`${selectedWorkspace.id}:${selected?.session_id ?? ""}`}
            client={client}
            workspace={selectedWorkspace}
            session={selected}
            busy={gitWorkspaceBusy}
            mutationDisabled={gitMutationDisabled}
            modal={compactViewport}
            onClose={() => {
              restoreGitFocus.current = true;
              setGitOpen(false);
            }}
          />
        </Suspense>}
      </Presence>
      <Presence open={conversationNavigatorOpen} kind="dialog">
        <Suspense fallback={null}>
          <ConversationNavigator
            items={conversationNavigation}
            currentEntryID={readerEntryID}
            hasEarlier={snapshot.historyMoreBefore}
            onClose={() => setConversationNavigatorOpen(false)}
            onSelect={jumpToNavigationItem}
            onLoadEarlier={async () => {
              pendingReadingRestoreRef.current = captureReadingPosition(true);
              return client.loadEarlierHistory();
            }}
          />
        </Suspense>
      </Presence>
      <Presence open={contextOpen} kind="dialog">
        <Suspense fallback={null}>
          <WorkspaceContextDialog
            snapshot={snapshot}
            client={client}
            onClose={closeContext}
            onError={reportLocalError}
          />
        </Suspense>
      </Presence>
      <Presence open={workspaceDialogOpen} kind="dialog">
        <WorkspaceDialog
          snapshot={snapshot}
          client={client}
          onRemove={setWorkspaceRemovalID}
          onClose={() => setWorkspaceDialogOpen(false)}
          onError={reportLocalError}
        />
      </Presence>
      <Presence open={Boolean(workspaceRemoval)} kind="dialog">
      {workspaceRemoval && (
        <WorkspaceRemovalDialog
          label={workspaceRemoval.label}
          busy={workspaceRemoving}
          onCancel={() => setWorkspaceRemovalID("")}
          onConfirm={() => void removeWorkspace(workspaceRemoval.id)}
        />
      )}
      </Presence>
      <Presence open={settingsOpen} kind="dialog">
        <Suspense fallback={null}>
          <SettingsDialog
            snapshot={snapshot}
            client={client}
            newIsolation={newIsolation}
            theme={themeMode}
            initialSection={settingsSection}
            initialAddModel={settingsAddModel}
            onIsolationChange={setNewIsolation}
            onThemeChange={setThemeMode}
            onClose={closeSettings}
            onError={reportLocalError}
          />
        </Suspense>
      </Presence>
    </div>
  );

}

function readTranscriptPosition(
  scrollport: HTMLElement,
  windowEndID: string | undefined,
  atBottom: boolean
): TranscriptReadingPosition | undefined {
  const anchors = Array.from(
    scrollport.querySelectorAll<HTMLElement>("[data-entry-id]")
  );
  if (anchors.length === 0) return undefined;
  const viewport = scrollport.getBoundingClientRect();
  const visible = visibleTranscriptAnchors(scrollport);
  const anchor = (atBottom ? visible.at(-1) : visible[0]) ??
    (atBottom ? anchors.at(-1) : anchors[0]);
  const entryID = anchor?.dataset.entryId;
  if (!anchor || !entryID) return undefined;
  return {
    entryID,
    top: anchorContent(anchor).getBoundingClientRect().top - viewport.top,
    scrollTop: scrollport.scrollTop,
    windowEndID,
    atBottom
  };
}

function visibleTranscriptAnchors(scrollport: HTMLElement): HTMLElement[] {
  const viewport = scrollport.getBoundingClientRect();
  const composer = scrollport.querySelector<HTMLElement>("[data-composer-seat]");
  const visibleBottom = Math.min(viewport.bottom, composer?.getBoundingClientRect().top ?? viewport.bottom);
  return Array.from(scrollport.querySelectorAll<HTMLElement>("[data-entry-id]"))
    .filter((anchor) => {
      const box = anchorContent(anchor).getBoundingClientRect();
      return box.height > 0 && box.bottom > viewport.top && box.top < visibleBottom;
    });
}

function transcriptAnchor(
  scrollport: HTMLElement,
  entryID: string
): HTMLElement | undefined {
  return Array.from(
    scrollport.querySelectorAll<HTMLElement>("[data-entry-id]")
  ).find((node) => node.dataset.entryId === entryID);
}

function transcriptFocusEntryID(scrollport: HTMLElement): string | undefined {
  const viewport = scrollport.getBoundingClientRect();
  const composer = scrollport.querySelector<HTMLElement>("[data-composer-seat]");
  const visibleBottom = composer?.getBoundingClientRect().top ?? viewport.bottom;
  const focusLine = viewport.top + (visibleBottom - viewport.top) * 0.42;
  let nearest: {id: string; distance: number} | undefined;
  for (const anchor of scrollport.querySelectorAll<HTMLElement>(
    "[data-entry-id]"
  )) {
    const id = anchor.dataset.entryId;
    if (!id) continue;
    const box = anchorContent(anchor).getBoundingClientRect();
    if (box.bottom <= viewport.top || box.top >= visibleBottom) continue;
    const center = box.top + Math.min(box.height, visibleBottom - box.top) / 2;
    const distance = Math.abs(center - focusLine);
    if (!nearest || distance < nearest.distance) nearest = {id, distance};
  }
  return nearest?.id;
}

function anchorContent(anchor: HTMLElement): HTMLElement {
  return anchor.firstElementChild instanceof HTMLElement
    ? anchor.firstElementChild
    : anchor;
}

function fileTarget(
  anchor: HTMLElement,
  path: string
): HTMLElement | undefined {
  return Array.from(
    anchor.querySelectorAll<HTMLElement>("[data-file-path]")
  ).find((node) => node.dataset.filePath === path);
}

function centerTranscriptTarget(
  scrollport: HTMLElement,
  target: HTMLElement
): void {
  const viewport = scrollport.getBoundingClientRect();
  const composer = scrollport.querySelector<HTMLElement>("[data-composer-seat]");
  const visibleBottom = composer?.getBoundingClientRect().top ?? viewport.bottom;
  const targetBox = target.getBoundingClientRect();
  const visibleHeight = Math.max(1, visibleBottom - viewport.top);
  const desiredTop = viewport.top +
    Math.max(16, (visibleHeight - Math.min(targetBox.height, visibleHeight)) / 2);
  const behavior = scrollport.style.scrollBehavior;
  scrollport.style.scrollBehavior = "auto";
  scrollport.scrollTop += targetBox.top - desiredTop;
  scrollport.style.scrollBehavior = behavior;
}

function restoreTranscriptPosition(
  scrollport: HTMLElement,
  position: TranscriptReadingPosition
): void {
  if (position.atBottom) {
    scrollport.scrollTop = scrollport.scrollHeight;
    return;
  }
  scrollport.scrollTop = position.scrollTop;
  const anchor = transcriptAnchor(scrollport, position.entryID);
  if (!anchor) return;
  const top = anchorContent(anchor).getBoundingClientRect().top -
    scrollport.getBoundingClientRect().top;
  scrollport.scrollTop += top - position.top;
}

function isEditableElement(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return target.isContentEditable ||
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement;
}

function SessionRow({
  session,
  active,
  onClick,
  onRename,
  onPin,
  onArchive,
  onDelete,
  actionPending,
  actionError
}: {
  session: SessionSummary;
  active: boolean;
  onClick: () => void;
  onRename: () => void;
  onPin: () => void;
  onArchive: () => void;
  onDelete: () => void;
  actionPending: boolean;
  actionError: string;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const actionsRef = useRef<HTMLDivElement>(null);
  const showStatus = session.status !== "idle";
  const statusTone = session.status === "failed"
    ? "error"
    : session.status === "awaiting_approval" ||
        session.status === "awaiting_input" ||
        session.status === "interrupted" ||
        session.status === "blocked"
      ? "warning"
      : session.status === "completed"
        ? "complete"
        : "active";
  const statusLabel = session.status === "awaiting_approval"
    ? "Approval required"
    : session.status === "awaiting_input"
      ? "Input required"
      : session.status === "blocked"
        ? "Blocked"
        : session.status === "interrupted"
          ? "Paused"
          : session.status === "failed"
            ? "Failed"
            : session.status === "completed"
              ? "Completed"
              : "Running";
  const StatusIcon = session.status === "running"
    ? LoaderCircle
    : session.status === "completed"
      ? Check
      : session.status === "blocked"
        ? AlertTriangle
        : session.status === "interrupted"
          ? CirclePause
          : AlertTriangle;
  const run = (action: () => void) => {
    setMenuOpen(false);
    actionsRef.current?.querySelector("button")?.focus();
    action();
  };
  return (
    <div
      className="sessionRow"
      data-active={active || undefined}
      data-menu-open={menuOpen || undefined}
      data-error={Boolean(actionError) || undefined}
      aria-busy={actionPending || undefined}
      role="treeitem"
      aria-selected={active}
      onMouseLeave={() => setMenuOpen(false)}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          setMenuOpen(false);
          actionsRef.current?.querySelector("button")?.focus();
        }
      }}
    >
      <button className="sessionSelect" onClick={onClick}>
        <span className="sessionStatusSlot">
          {showStatus && (
            <span
              key={session.status}
              className="sessionStatusMark"
              data-tone={statusTone}
              title={statusLabel}
              role="img"
              aria-label={statusLabel}
            >
              <StatusIcon
                className={session.status === "running" ? "spin" : undefined}
                size={12}
              />
            </span>
          )}
        </span>
        <span className="sessionTitle">{session.title}</span>
        <span className="sessionAge">{relativeTime(session.updated_at)}</span>
      </button>
      <div className="sessionActions" ref={actionsRef}>
        <IconButton
          label={`Session actions for ${session.title}`}
          icon={actionPending
            ? <LoaderCircle className="spin" size={14} />
            : <MoreHorizontal size={15} />}
          disabled={actionPending}
          expanded={menuOpen}
          onClick={() => setMenuOpen((value) => !value)}
        />
        <Presence open={menuOpen}>
          <div className="sessionMenu" data-motion-surface role="menu">
            <button role="menuitem" onClick={() => run(onRename)}>
              <Pencil size={14} /> Rename
            </button>
            <button role="menuitem" onClick={() => run(onPin)}>
              {session.pinned ? <PinOff size={14} /> : <Pin size={14} />}
              {session.pinned ? "Unpin" : "Pin"}
            </button>
            <button role="menuitem" onClick={() => run(onArchive)}>
              <Archive size={14} />
              {session.archived ? "Restore" : "Archive"}
            </button>
            <button
              className="dangerMenuItem"
              role="menuitem"
              onClick={() => run(onDelete)}
            >
              <Trash2 size={14} /> Delete
            </button>
          </div>
        </Presence>
      </div>
      {actionError && (
        <span className="sessionActionError" role="alert">
          <AlertTriangle size={13} aria-hidden="true" />
          <span>{actionError}</span>
        </span>
      )}
    </div>
  );
}

function sessionIsBusy(session: SessionSummary): boolean {
  return session.status === "running" ||
    session.status === "awaiting_approval" ||
    session.status === "awaiting_input";
}

function DisclosureLeading({
  open,
  icon
}: {
  open: boolean;
  icon: ReactNode;
}) {
  return (
    <span className="disclosureLeading" aria-hidden="true">
      <span className="disclosureIcon">{icon}</span>
      <span className="disclosureChevron">
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
      </span>
    </span>
  );
}

type TerminalTurnKind = "completed" | "failed";

interface TranscriptTurn {
  readonly id: string;
  readonly turnID: string;
  readonly entries: readonly ConversationNode[];
}

function groupTranscriptTurns(
  entries: readonly ConversationNode[]
): readonly TranscriptTurn[] {
  const turns: Array<Omit<TranscriptTurn, "entries"> & {entries: ConversationNode[]}> = [];
  for (const entry of entries) {
    const previous = turns.at(-1);
    if (previous?.turnID === entry.turnID) {
      previous.entries.push(entry);
      continue;
    }
    turns.push({
      id: `${entry.turnID}:${entry.id}`,
      turnID: entry.turnID,
      entries: [entry]
    });
  }
  return turns;
}

function terminalTurnKinds(
  events: readonly RuntimeEvent[]
): ReadonlyMap<string, TerminalTurnKind> {
  const result = new Map<string, TerminalTurnKind>();
  for (const event of events) {
    if (event.kind === "turn.completed") {
      result.set(event.turn_id, "completed");
    } else if (event.kind === "turn.failed" || event.kind === "turn.canceled") {
      result.set(event.turn_id, "failed");
    }
  }
  return result;
}

function terminalConclusion(
  entries: readonly ConversationNode[],
  terminalKind: TerminalTurnKind
): ConversationNode | undefined {
  if (terminalKind === "completed") {
    return [...entries].reverse().find(
      (entry): entry is Extract<ConversationNode, {kind: "assistant"}> =>
        entry.kind === "assistant" && !entry.superseded
    );
  }
  return [...entries].reverse().find(
    (entry): entry is Extract<ConversationNode, {kind: "status"}> =>
      entry.kind === "status"
  );
}

function TurnTranscript({
  entries,
  terminalKind,
  preserveReading,
  revealEntryID,
  client,
  onError,
  onInspect,
  checkpoints,
  recoveryTurnID,
  canWithdraw,
  withdrawalCommitted,
  chrome,
  messageFeedback,
  selectedSessionID,
  navigationHighlightID
}: {
  entries: readonly ConversationNode[];
  terminalKind?: TerminalTurnKind;
  preserveReading: boolean;
  revealEntryID?: string;
  client: RuntimeClient;
  onError: (error: unknown) => void;
  onInspect: (callID: string) => void;
  checkpoints: readonly SessionCheckpoint[];
  recoveryTurnID?: string;
  canWithdraw?: boolean;
  withdrawalCommitted?: boolean;
  chrome?: MessageChrome;
  messageFeedback: RuntimeSnapshot["messageFeedback"];
  selectedSessionID: string;
  navigationHighlightID: string;
}) {
  const [layout, setLayout] = useState<"live" | "reading" | "settled">(
    terminalKind ? "settled" : "live"
  );
  // Decide before committing DOM: moving an inspected entry into a different
  // subtree would discard its disclosure state, focus and selection.
  if (layout === "live" && terminalKind) {
    setLayout(preserveReading ? "reading" : "settled");
  } else if (layout === "reading" && !preserveReading) {
    setLayout("settled");
  }
  const summarizeExecution = Boolean(terminalKind) && layout === "settled";
  const conclusion = terminalKind
    ? terminalConclusion(entries, terminalKind)
    : undefined;
  const executionEntries = summarizeExecution
    ? entries.filter((entry) => entry.kind !== "user" && entry.id !== conclusion?.id)
    : [];
  const visibleEntries = summarizeExecution
    ? entries.filter((entry) => entry.kind === "user" || entry.id === conclusion?.id)
    : entries;
  const revealExecution = Boolean(
    revealEntryID && executionEntries.some((entry) => entry.id === revealEntryID)
  );
  const [executionOpen, setExecutionOpen] = useState(false);
  const [withdrawnOpen, setWithdrawnOpen] = useState(false);
  const withdrawn = withdrawalCommitted ||
    entries.some((entry) => entry.kind === "user" && entry.withdrawn);
  const requestEntryID = entries.find((entry) => entry.kind === "user" && !entry.steering)?.id;
  const revealWithdrawn = Boolean(withdrawn && revealEntryID &&
    entries.some((entry) => entry.id === revealEntryID));

  useEffect(() => {
    if (revealExecution) setExecutionOpen(true);
  }, [revealExecution]);

  useEffect(() => {
    if (revealWithdrawn) setWithdrawnOpen(true);
  }, [revealWithdrawn, revealEntryID]);

  const renderEntry = (entry: ConversationNode) => (
    <TranscriptEntry
      key={entry.id}
      entry={withdrawn && entry.kind === "status"
        ? {...entry, recoverable: false, recovery: undefined}
        : entry}
      client={client}
      onError={onError}
      onInspect={onInspect}
      checkpoint={checkpointForTurn(checkpoints, entry.turnID)}
      recoveryTurnID={recoveryTurnID}
      canWithdraw={entry.id === requestEntryID && canWithdraw && !withdrawn}
      withdrawn={entry.id === requestEntryID && withdrawn}
      chrome={chrome}
      feedback={messageFeedback[`${selectedSessionID}:${entry.id}`]}
      navigationHighlightID={navigationHighlightID}
    />
  );

  return (
    <section className="turnTranscript" data-turn-id={entries[0]?.turnID}>
      {withdrawn && (
        <button type="button" className="withdrawnTurnToggle"
          aria-expanded={withdrawnOpen} onClick={() => setWithdrawnOpen((value) => !value)}>
          {withdrawnOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          <span>Turn withdrawn</span>
          <small>Excluded from context</small>
        </button>
      )}
      {(!withdrawn || withdrawnOpen) && <>
      <ExecutionStages entries={visibleEntries} revealEntryID={revealEntryID} renderEntry={renderEntry} />
      {executionEntries.length > 0 && (
        <div
          className="turnExecution"
          data-expanded={executionOpen || undefined}
        >
          <button
            type="button"
            className="turnExecutionToggle"
            aria-expanded={executionOpen}
            onClick={() => setExecutionOpen((value) => !value)}
          >
            {executionOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
            <span>Execution details</span>
            <small>{executionEntries.length} steps</small>
          </button>
          <Collapse open={executionOpen}>
            <div className="turnExecutionItems">
              <ExecutionStages entries={executionEntries} revealEntryID={revealEntryID} renderEntry={renderEntry} />
            </div>
          </Collapse>
        </div>
      )}
      </>}
    </section>
  );
}

function TranscriptEntry({
  entry,
  client,
  onError,
  onInspect,
  checkpoint,
  recoveryTurnID,
  canWithdraw,
  withdrawn,
  chrome,
  feedback,
  navigationHighlightID
}: {
  entry: ConversationNode;
  client: RuntimeClient;
  onError: (error: unknown) => void;
  onInspect: (callID: string) => void;
  checkpoint?: SessionCheckpoint;
  recoveryTurnID?: string;
  canWithdraw?: boolean;
  withdrawn?: boolean;
  chrome?: MessageChrome;
  feedback?: MessageFeedbackRating;
  navigationHighlightID: string;
}) {
  return (
    <div
      className="transcriptEntryAnchor"
      data-entry-id={entry.id}
      data-entry-kind={entry.kind}
      data-turn-id={entry.turnID || undefined}
      data-navigation-current={navigationHighlightID === entry.id || undefined}
    >
      <TranscriptItem
        entry={entry}
        client={client}
        onError={onError}
        onInspect={onInspect}
        checkpoint={checkpoint}
        recoveryTurnID={recoveryTurnID}
        canWithdraw={canWithdraw}
        withdrawn={withdrawn}
        chrome={chrome}
        feedback={feedback}
      />
    </div>
  );
}

const TranscriptItem = memo(function TranscriptItem({
  entry,
  client,
  onError,
  onInspect,
  checkpoint,
  recoveryTurnID,
  canWithdraw,
  withdrawn,
  chrome,
  feedback
}: {
  entry: ConversationNode;
  client: RuntimeClient;
  onError: (error: unknown) => void;
  onInspect: (callID: string) => void;
  checkpoint?: SessionCheckpoint;
  recoveryTurnID?: string;
  canWithdraw?: boolean;
  withdrawn?: boolean;
  chrome?: MessageChrome;
  feedback?: MessageFeedbackRating;
}) {
  const [open, setOpen] = useState(false);
  const [recoveryPending, setRecoveryPending] = useState("");
  const onFeedback = useCallback(
    (rating: MessageFeedbackRating) => client.toggleMessageFeedback(entry.id, rating),
    [client, entry.id]
  );
  if (entry.kind === "user") {
    return (
      <div className="userMessageGroup">
        <div
          className="userMessage"
          data-steering={entry.steering || undefined}
        >
          {entry.steering && <small>Steered</small>}
          {entry.images.length > 0 && (
            <div className="userMessageImages">
              {entry.images.map((image, index) => (
                <img
                  src={`data:${image.mediaType};base64,${image.content}`}
                  alt={image.label}
                  loading="lazy"
                  key={`${image.label}:${index}`}
                />
              ))}
            </div>
          )}
          {entry.text && <span>{entry.text}</span>}
        </div>
        {withdrawn ? (
          <div className="userMessageActions">Withdrawn</div>
        ) : canWithdraw ? (
          <TurnWithdrawalAction client={client} turnID={entry.turnID} />
        ) : null}
      </div>
    );
  }
  if (entry.kind === "commentary") {
    return (
      <article className="assistantMessage commentaryMessage">
        <Suspense fallback={
          <div className="assistantMarkdownFallback">{entry.text}</div>
        }>
          <MarkdownMessage
            text={entry.text}
            settled
          />
        </Suspense>
      </article>
    );
  }
  if (entry.kind === "assistant") {
    return (
      <article
        className="assistantMessage"
        data-superseded={entry.superseded || undefined}
        data-time-hover-root
      >
        <Suspense fallback={
          <div className="assistantMarkdownFallback">{entry.text}</div>
        }>
          <MarkdownMessage
            text={entry.text}
            settled={Boolean(chrome)}
          />
        </Suspense>
        {chrome && !entry.superseded && (
          <MessageActions
            text={entry.text}
            chrome={chrome}
            feedback={feedback}
            onFeedback={onFeedback}
          />
        )}
      </article>
    );
  }
  if (entry.kind === "status") {
    return (
      <div
        className="terminalState"
        data-failed={entry.failed || undefined}
        data-warning={entry.warning || undefined}
      >
        {entry.title === "Paused"
          ? <CirclePause size={16} />
          : entry.failed || entry.blocked
            ? <AlertTriangle size={16} />
            : entry.warning
              ? <LoaderCircle size={16} />
              : <Check size={16} />}
        <div><strong>{entry.title}</strong><span>{entry.text}</span></div>
        {entry.recoverable &&
          (!recoveryTurnID || entry.turnID === recoveryTurnID) &&
          entry.recovery && (
          <div className="turnRecovery">
            <span className="turnRecoveryStatus">
              {recoverySummary(entry.recovery.sideEffects)}
            </span>
            <div className="artifactActions">
              {entry.recovery.canRetry && (
                <button
                  disabled={Boolean(recoveryPending)}
                  onClick={() => {
                    setRecoveryPending("retry");
                    void client.recoverTurn(entry.turnID, "retry")
                      .catch(onError)
                      .finally(() => setRecoveryPending(""));
                  }}
                >
                  <RotateCcw size={13} />
                  Retry
                </button>
              )}
              {entry.recovery.canContinue && (
                <button
                  disabled={Boolean(recoveryPending)}
                  onClick={() => {
                    setRecoveryPending("continue");
                    void client.recoverTurn(entry.turnID, "continue")
                      .catch(onError)
                      .finally(() => setRecoveryPending(""));
                  }}
                >
                  <Play size={13} />
                  Continue
                </button>
              )}
              {checkpoint?.can_restore && (
                <button
                  disabled={Boolean(recoveryPending)}
                  onClick={() => {
                    setRecoveryPending("restore");
                    void client.restoreCheckpoint(checkpoint.id)
                      .catch(onError)
                      .finally(() => setRecoveryPending(""));
                  }}
                >
                  <RefreshCw size={13} /> Restore
                </button>
              )}
              {checkpoint?.can_fork && (
                <button
                  disabled={Boolean(recoveryPending)}
                  onClick={() => {
                    setRecoveryPending("fork");
                    void client.forkCheckpoint(checkpoint.id)
                      .catch(onError)
                      .finally(() => setRecoveryPending(""));
                  }}
                >
                  <GitFork size={13} /> Fork
                </button>
              )}
            </div>
          </div>
        )}
      </div>
    );
  }
  if (entry.kind === "receipt" || entry.kind === "deliverables") {
    return null;
  }
  if (entry.kind === "context") {
    return (
      <div className="disclosure contextDisclosure">
        <button onClick={() => setOpen((value) => !value)} aria-expanded={open}>
          <DisclosureLeading open={open} icon={<FileCode2 size={14} />} />
          <span className="disclosureTitle">{entry.title}</span>
          <span className="disclosureSeparator" aria-hidden="true" />
          <small>{entry.summary}</small>
        </button>
        {open && <pre>{pretty(entry.data)}</pre>}
      </div>
    );
  }
  if (entry.kind === "reasoning") {
    return <ReasoningDisclosure entry={entry} />;
  }
  if (entry.kind === "agent") {
    return <AgentDisclosure entry={entry} onInspect={onInspect} />;
  }
  return <ToolDisclosure
    entry={entry}
    onInspect={onInspect}
    onAddContext={(callID, text) => {
      void client.addTerminalContext(callID, text).catch(onError);
    }}
  />;
});

function recoverySummary(sideEffects: string): string {
  switch (sideEffects) {
    case "draft":
      return "Draft saved. Continue from the last durable step.";
    case "committed":
      return "Workspace changes were kept.";
    case "rolled_back":
      return "Workspace changes were rolled back.";
    case "none":
      return "No workspace changes were made.";
    default:
      return "Review the workspace before continuing.";
  }
}

function TurnStatus({
  events,
  turnID,
  status
}: {
  events: readonly RuntimeEvent[];
  turnID: string;
  status?: string;
}) {
  const startedAt = useMemo(() => {
    const value = events.find(
      (event) => event.turn_id === turnID && event.kind === "turn.started"
    )?.created_at;
    const parsed = value ? Date.parse(value) : Number.NaN;
    return Number.isFinite(parsed) ? parsed : Date.now();
  }, [events, turnID]);
  const [elapsed, setElapsed] = useState(() => Math.max(0, Date.now() - startedAt));
  useEffect(() => {
    const tick = () => setElapsed(Math.max(0, Date.now() - startedAt));
    tick();
    const interval = window.setInterval(tick, 1_000);
    return () => window.clearInterval(interval);
  }, [startedAt]);
  return (
    <div className="turnStatus" role="status" aria-live="polite">
      <span>{status ?? "Thinking..."}</span>
      {elapsed >= 15_000 && <small>{formatDuration(elapsed)}</small>}
    </div>
  );
}

function ApprovalComposer({
  event,
  client,
  stopping,
  onStop
}: {
  event: RuntimeEvent;
  client: RuntimeClient;
  stopping: boolean;
  onStop: () => void;
}) {
  const [replacement, setReplacement] = useState("");
  const [replacementOpen, setReplacementOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const data = event.data;
  const requestID = String(data.request_id ?? "");
  const planID = typeof data.edit_plan === "object" && data.edit_plan
    ? String((data.edit_plan as Record<string, unknown>).id ?? "")
    : "";
  const editPlan = projectEditPlan(data.edit_plan);
  const scopes = Array.isArray(data.allowed_scopes)
    ? data.allowed_scopes.map(String)
    : [];
  const replacementAllowed = Boolean(data.replacement_allowed);
  const replacementArguments = parseJSONObject(replacement);
  const replacementValid = !replacement.trim() || replacementArguments !== undefined;
  const decide = async (
    decision: "approve" | "deny" | "cancel",
    approvalScope = ""
  ) => {
    if (submitting || (decision === "approve" && !replacementValid)) return;
    setSubmitting(true);
    setError("");
    try {
      await client.decideApproval(
        requestID,
        decision,
        planID,
        approvalScope,
        replacementArguments
      );
    } catch (value) {
      setError(value instanceof Error ? value.message : String(value));
      setSubmitting(false);
    }
  };
  return (
    <div className="pendingComposer approvalComposer" data-approval-key={requestID}>
      <div className="approvalStrip">
        <span className="approvalDot" />
        <strong>Waiting for approval</strong>
        <IconButton
          label="Stop turn"
          danger
          disabled={submitting || stopping}
          icon={stopping
            ? <LoaderCircle className="spin" size={16} />
            : <CircleStop size={16} />}
          onClick={onStop}
        />
      </div>
      <div
        className="approvalBody"
        tabIndex={0}
        role="group"
        aria-label="Approval details"
      >
        <div className="approvalHeadline">
          {editPlan
            ? `Review ${editPlan.files.length} file ${
              editPlan.files.length === 1 ? "change" : "changes"
            }`
            : `${String(data.tool ?? "Action")} requires approval`}
        </div>
        <div className="approvalReason">
          {String(data.effect ?? data.risk ?? "Review the requested effect.")}
        </div>
        {approvalCommand(data.arguments) && (
          <code className="approvalCommand">{approvalCommand(data.arguments)}</code>
        )}
        {editPlan && (
          <EditPlanPreview files={editPlan.files} diff={editPlan.diff} />
        )}
        <div className="pendingMeta">
          {planID && <span>Plan {planID.slice(0, 12)}</span>}
          {Array.isArray(data.resources) && (
            <span>{data.resources.length} protected resources</span>
          )}
          {typeof data.expires_at === "string" &&
            Date.parse(data.expires_at) > 0 && (
            <span>Expires {new Date(data.expires_at).toLocaleString()}</span>
          )}
        </div>
        {error && <span className="composerError">{error}</span>}
        {replacementAllowed && (
          <details className="approvalOptions">
            <summary>Approval options <ChevronDown size={13} /></summary>
            <button
              type="button"
              onClick={() => setReplacementOpen((value) => !value)}
            >
              <Braces size={14} />
              {replacementOpen ? "Hide replacement arguments" : "Edit arguments"}
            </button>
            {replacementOpen && (
              <label className="replacementEditor">
                <span>Replacement arguments (JSON)</span>
                <textarea
                  aria-label="Replacement arguments"
                  placeholder={'{"argument": "value"}'}
                  value={replacement}
                  aria-invalid={!replacementValid}
                  disabled={submitting}
                  onChange={(event) => setReplacement(event.target.value)}
                />
                {!replacementValid && (
                  <span className="composerError">Enter a JSON object.</span>
                )}
              </label>
            )}
          </details>
        )}
      </div>
      <div className="pendingActions approvalActions">
        <button type="button" disabled={submitting} onClick={() => void decide("deny")}>
          Deny
        </button>
        {scopes.includes("always") && (
          <button
            type="button"
            disabled={submitting || !replacementValid}
            onClick={() => void decide("approve", "always")}
          >
            Always approve
          </button>
        )}
        {scopes.includes("session") && (
          <button
            type="button"
            disabled={submitting || !replacementValid}
            onClick={() => void decide("approve", "session")}
          >
            Approve for session
          </button>
        )}
        <button
          type="button"
          className="primaryText"
          disabled={submitting || !replacementValid}
          onClick={() => void decide("approve", "once")}
        >
          Approve once
        </button>
      </div>
    </div>
  );
}

function InputComposer({
  event,
  client,
  stopping,
  onStop
}: {
  event: RuntimeEvent;
  client: RuntimeClient;
  stopping: boolean;
  onStop: () => void;
}) {
  const [answer, setAnswer] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const requestID = String(event.data.request_id ?? "");
  const options = Array.isArray(event.data.options)
    ? event.data.options.map(String)
    : [];
  const submit = async () => {
    const value = answer.trim();
    if (!value || submitting) return;
    setSubmitting(true);
    setError("");
    try {
      await client.replyInput(
        requestID,
        value,
        options.includes(value) ? {selection: value} : undefined
      );
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
      setSubmitting(false);
    }
  };
  return (
    <div className="pendingComposer inputComposer">
      <strong>{String(event.data.prompt ?? "Input required")}</strong>
      {error && <span className="composerError">{error}</span>}
      <div className="pendingActions inputResponse">
        {options.length > 0 && (
          <InputOptionMenu
            value={answer}
            options={options}
            disabled={submitting}
            onChange={setAnswer}
          />
        )}
        <input
          aria-label="Custom input answer"
          placeholder="Or enter a custom answer"
          value={answer}
          autoFocus
          disabled={submitting}
          onChange={(value) => setAnswer(value.target.value)}
        />
        <IconButton
          label="Stop turn"
          danger
          disabled={submitting || stopping}
          icon={stopping
            ? <LoaderCircle className="spin" size={17} />
            : <CircleStop size={17} />}
          onClick={onStop}
        />
        <button
          className="primaryText"
          disabled={!answer.trim() || submitting}
          onClick={() => void submit()}
        >
          Submit
        </button>
      </div>
    </div>
  );
}

function WorkspaceDialog({
  snapshot,
  client,
  onRemove,
  onClose,
  onError
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  onRemove: (workspaceID: string) => void;
  onClose: () => void;
  onError: (error: unknown) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const dialogRef = useRef<HTMLElement>(null);
  useModalFocus(dialogRef, true, onClose);
  const choose = async () => {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const selection = await client.pickWorkspaceDirectory();
      if (selection.cancelled) return;
      if (!selection.path) throw new Error("No folder was selected");
      await client.addWorkspace(selection.path);
      onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
      onError(reason);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div
      className="contextDialogOverlay"
      data-motion-backdrop
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        data-motion-surface
        className="contextDialog workspaceDialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="workspace-dialog-title"
      >
        <header className="contextDialogHeader">
          <div>
            <h2 id="workspace-dialog-title">Workspaces</h2>
          </div>
          <IconButton
            label="Close workspace selector"
            icon={<X size={17} />}
            onClick={onClose}
          />
        </header>
        <div className="contextResults workspaceChoices">
          {snapshot.workspaces.map((workspace) => (
            <div
              className="workspaceHeader"
              key={workspace.id}
              data-active={
                workspace.id === snapshot.selectedWorkspaceID || undefined
              }
            >
              <button
                className="workspaceRow"
                disabled={!workspace.ready || busy}
                onClick={() => void client.selectWorkspace(workspace.id)
                  .then(onClose)
                  .catch((reason) => {
                    setError(reason instanceof Error ? reason.message : String(reason));
                    onError(reason);
                  })}
              >
                <FolderOpen size={16} />
                <span className="sessionTitle">{workspace.label}</span>
                <small>{workspace.ready
                  ? `${workspace.session_count} sessions`
                  : workspace.problem || "Starting"}</small>
              </button>
              <WorkspaceRemovalButton
                id={workspace.id}
                label={workspace.label}
                removable={workspace.removable}
                disabled={busy}
                onRemove={onRemove}
              />
            </div>
          ))}
        </div>
        <div className="workspaceAdd">
          <button
            className="startupCreate"
            type="button"
            disabled={busy}
            onClick={() => void choose()}
          >
            {busy
              ? <LoaderCircle className="spin" size={16} />
              : <FolderPlus size={16} />}
            <span>{busy ? "Working..." : "Choose folder"}</span>
          </button>
        </div>
        {error && (
          <p className="startupError workspaceDialogError" role="alert">
            {error}
          </p>
        )}
      </section>
    </div>
  );
}

function WorkspaceRemovalDialog({
  label,
  busy,
  onCancel,
  onConfirm
}: {
  label: string;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  useModalFocus(dialogRef, true, () => { if (!busy) onCancel(); });
  return (
    <div className="contextDialogOverlay" data-motion-backdrop>
      <section
        ref={dialogRef}
        data-motion-surface
        className="contextDialog workspaceRemovalDialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="workspace-removal-title"
        aria-describedby="workspace-removal-description"
      >
        <header className="contextDialogHeader">
          <div>
            <h2 id="workspace-removal-title">Remove workspace?</h2>
          </div>
        </header>
        <p id="workspace-removal-description" className="workspaceRemovalMessage">
          <strong>{label}</strong> will be removed from QCode. Files and
          session data remain on disk.
        </p>
        <div className="workspaceRemovalActions">
          <button type="button" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          <button
            type="button"
            className="workspaceRemovalConfirm"
            disabled={busy}
            onClick={onConfirm}
          >
            {busy ? <LoaderCircle className="spin" size={14} /> : <Trash2 size={14} />}
            Remove workspace
          </button>
        </div>
      </section>
    </div>
  );
}

function WorkspaceRemovalButton({
  id,
  label,
  removable,
  disabled,
  onRemove
}: {
  id: string;
  label: string;
  removable: boolean;
  disabled?: boolean;
  onRemove: (workspaceID: string) => void;
}) {
  if (!removable) return null;
  return (
    <IconButton
      label={`Remove ${label}`}
      icon={<Trash2 size={14} />}
      danger
      disabled={disabled}
      onClick={() => onRemove(id)}
    />
  );
}

function FirstRunSetup({
  catalog,
  workspaceRoot,
  client
}: {
  catalog: SetupCatalog;
  workspaceRoot: string;
  client: RuntimeClient;
}) {
  const [providerID, setProviderID] = useState("");
  const [modelID, setModelID] = useState("");
  const [apiKey, setAPIKey] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [protocol, setProtocol] = useState("openai_chat");
  const [metadata, setMetadata] = useState(emptyModelMetadataDraft);
  const [probed, setProbed] = useState(false);
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const provider = catalog.providers.find((entry) => entry.id === providerID);
  const custom = Boolean(provider?.custom);
  const requiresMetadata = Boolean(
    provider && modelID.trim() &&
    (custom || !provider.models?.includes(modelID.trim()))
  );
  const metadataError = requiresMetadata && probed
    ? modelMetadataProblem(
        metadata,
        custom ? protocol : provider?.protocol ?? ""
      )
    : "";
  const keyError = apiKeyError(apiKey);
  const ready = Boolean(
    provider &&
    modelID.trim() &&
    (!provider.requires_api_key || apiKey) &&
    (!custom || baseURL.trim()) &&
    (!requiresMetadata || probed) &&
    !metadataError &&
    !keyError
  );

  const submit = async () => {
    if (!ready || submitting) return;
    const request: SetupRequest = {
      provider: providerID,
      model: modelID.trim(),
      api_key: apiKey,
      ...(custom ? {base_url: baseURL.trim(), protocol} : {}),
      ...(requiresMetadata ? {model_metadata: setupModelMetadata(metadata)} : {})
    };
    setSubmitting(true);
    setError("");
    try {
      await client.completeSetup(request);
      setAPIKey("");
    } catch (value) {
      setError(value instanceof Error ? value.message : String(value));
      setSubmitting(false);
    }
  };
  const probeModel = async () => {
    if (!provider || (custom && !baseURL.trim()) || !modelID.trim() || probing) return;
    setProbing(true);
    setProbeError("");
    try {
      const result = await client.probeSetup({
        provider: providerID,
        base_url: custom ? baseURL.trim() : "",
        protocol: custom ? protocol : provider.protocol,
        model: modelID.trim(),
        ...(apiKey.trim() ? {api_key: apiKey.trim()} : {})
      });
      setMetadata(modelMetadataFromProbe(modelID.trim(), result));
      setProbed(true);
      setProbeError(result.warning ?? "");
    } catch (value) {
      setProbed(false);
      setProbeError(value instanceof Error ? value.message : String(value));
    } finally {
      setProbing(false);
    }
  };

  return (
    <main className="setupPage">
      <form
        className="startupSetup"
        aria-labelledby="startup-title"
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <div className="startupHeading">
          <div className="emptyMark"><CapybaraMark size="hero" /></div>
          <div>
            <h1 id="startup-title">Set up QCode</h1>
            <p>Model connection</p>
          </div>
        </div>

        <div className="startupSection">
          <div className="startupSectionHeading">
            <span>1</span>
            <strong>Provider</strong>
          </div>
          <div className="startupFields">
            <label className="selectField">
              <span>Provider</span>
              <select
                aria-label="Provider"
                value={providerID}
                autoFocus
                disabled={submitting || probing}
                onChange={(event) => {
                  const next = catalog.providers.find(
                    (entry) => entry.id === event.target.value
                  );
                  setProviderID(event.target.value);
                  setModelID("");
                  setBaseURL("");
                  setProtocol(next?.protocol || "openai_chat");
                  setMetadata(emptyModelMetadataDraft());
                  setProbed(false);
                  setProbeError("");
                  setError("");
                }}
              >
                <option value="" disabled>Select provider</option>
                {catalog.providers.map((entry) => (
                  <option value={entry.id} key={entry.id}>
                    {entry.display_name}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </div>

        {provider && (
          <div className="startupSection">
            <div className="startupSectionHeading">
              <span>2</span>
              <strong>Model</strong>
            </div>
            <div className="startupFields" data-custom={custom || undefined}>
              {custom && (
                <label className="selectField">
                  <span>Base URL</span>
                  <input
                    aria-label="Base URL"
                    value={baseURL}
                    placeholder="https://api.example.com/v1"
                    disabled={submitting || probing}
                    onChange={(event) => {
                      setBaseURL(event.target.value);
                      setProbed(false);
                    }}
                  />
                </label>
              )}
              {custom && (
                <label className="selectField">
                  <span>Protocol</span>
                  <select
                    aria-label="Protocol"
                    value={protocol}
                    disabled={submitting || probing}
                    onChange={(event) => {
                      setProtocol(event.target.value);
                      setProbed(false);
                    }}
                  >
                    <option value="openai_chat">Chat Completions</option>
                    <option value="openai_responses">Responses</option>
                  </select>
                </label>
              )}
              <label className="selectField">
                <span>Model ID</span>
                <input
                  aria-label="Model ID"
                  value={modelID}
                  placeholder="Enter the exact model ID"
                  disabled={submitting || probing}
                  onChange={(event) => {
                    const nextModelID = event.target.value;
                    setMetadata(emptyModelMetadataDraft(nextModelID.trim()));
                    setModelID(nextModelID);
                    setProbed(false);
                  }}
                />
              </label>
              {requiresMetadata && probed && (
                <ModelMetadataFields
                  value={metadata}
                  disabled={submitting}
                  onChange={setMetadata}
                />
              )}
            </div>
            {metadataError && (
              <small className="startupError" role="alert">
                {metadataError}
              </small>
            )}
          </div>
        )}

        {provider && (
          <div className="startupSection">
            <div className="startupSectionHeading">
              <span>3</span>
              <strong>API key</strong>
              {!provider.requires_api_key && <small data-ready>Optional</small>}
            </div>
            <div className="startupCredential">
              <KeyRound size={16} aria-hidden="true" />
              <input
                type="password"
                autoComplete="off"
                aria-label="API key"
                value={apiKey}
                placeholder={provider.requires_api_key
                  ? "Enter API key"
                  : "Optional API key"}
                disabled={submitting || probing}
                onChange={(event) => {
                  setAPIKey(event.target.value);
                  setProbed(false);
                }}
              />
            </div>
            <p className="startupNote">
              Encrypted and stored by the operating system Keyring. Never written
              to this project, a config file, or browser storage.
            </p>
            {keyError && <p className="startupError">{keyError}</p>}
            {requiresMetadata && (
              <button
                type="button"
                className="settingsHeaderAction"
                disabled={
                  probing || (custom && !baseURL.trim()) || !modelID.trim() || Boolean(keyError)
                }
                onClick={() => void probeModel()}
              >
                {probing ? "Detecting..." : "Detect model"}
              </button>
            )}
            {probeError && <p className="startupError">{probeError}</p>}
            {requiresMetadata && !probed && (
              <button type="button" className="settingsHeaderAction" disabled={submitting || probing}
                onClick={() => {
                  setMetadata(emptyModelMetadataDraft(modelID.trim()));
                  setProbed(true);
                }}>
                Enter model metadata
              </button>
            )}
          </div>
        )}

        <div className="startupFooter">
          {workspaceRoot && <small title={workspaceRoot}>{workspaceRoot}</small>}
          <button className="startupCreate" disabled={!ready || submitting || probing}>
            {submitting
              ? <LoaderCircle className="spin" size={17} />
              : <Plus size={17} />}
            <span>{submitting ? "Starting..." : "Start QCode"}</span>
          </button>
        </div>
        {error && <p className="startupError" role="alert">{error}</p>}
      </form>
    </main>
  );
}

function EmptySessionSetup({
  creating,
  workspaceReady,
  onCreate,
  onChooseWorkspace
}: {
  creating: boolean;
  workspaceReady: boolean;
  onCreate: () => void;
  onChooseWorkspace: () => void;
}) {
  return (
    <section className="startupSetup emptySessionSetup" aria-labelledby="startup-title">
      <div className="startupHeading">
        <div className="emptyMark"><CapybaraMark size="hero" /></div>
        <div>
          <h2 id="startup-title">
            {workspaceReady ? "Start a new session" : "Choose a workspace"}
          </h2>
        </div>
      </div>
      <div className="startupFooter">
        <button
          className="startupCreate"
          disabled={creating}
          onClick={workspaceReady ? onCreate : onChooseWorkspace}
        >
          {creating
            ? <LoaderCircle className="spin" size={17} />
            : workspaceReady
              ? <Plus size={17} />
              : <FolderOpen size={17} />}
          <span>{creating
            ? "Creating..."
            : workspaceReady
              ? "Create session"
              : "Choose workspace"}</span>
        </button>
      </div>
    </section>
  );
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

function CompactSelect({
  label,
  value,
  values,
  disabled,
  onChange
}: {
  label: string;
  value: string;
  values: string[];
  disabled?: boolean;
  onChange: (value: string) => void;
}) {
  return (
    <label className="compactSelect" title={`${label}: ${value || "Default"}`}>
      <span className="srOnly">{label}</span>
      <select
        aria-label={label}
        value={value}
        style={compactSelectWidth(value || "Default")}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
      >
        {values.map((item) => (
          <option value={item} key={item}>{item || "Default"}</option>
        ))}
      </select>
      <ChevronDown size={13} aria-hidden="true" />
    </label>
  );
}

function CompactCatalogSelect({
  label,
  value,
  options,
  disabled,
  onChange
}: {
  label: string;
  value: string;
  options: CatalogOption[];
  disabled?: boolean;
  onChange: (value: string) => void;
}) {
  return (
    <label className="compactSelect" title={`${label}: ${value}`}>
      <span className="srOnly">{label}</span>
      <select
        aria-label={label}
        value={value}
        style={compactSelectWidth(
          options.find((option) => option.value === value)?.label ?? value
        )}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
      >
        {options.map((option) => (
          <option
            value={option.value}
            key={option.value}
            disabled={option.disabled}
          >
            {option.label}
          </option>
        ))}
      </select>
      <ChevronDown size={13} aria-hidden="true" />
    </label>
  );
}

function ComposerStats({
  receipt,
  usage,
  toolCalls
}: {
  receipt?: Readonly<Record<string, unknown>>;
  usage?: RuntimeSnapshot["usage"];
  toolCalls: number;
}) {
  if (!receipt && (!usage || usage.turns === 0)) {
    return <div className="composerMeta" />;
  }
  const latency = isObject(receipt?.latency) ? receipt.latency : undefined;
  const input = numberValue(receipt?.input_tokens);
  const output = numberValue(receipt?.output_tokens);
  const reasoning = numberValue(receipt?.reasoning_tokens);
  const cached = numberValue(receipt?.cached_tokens);
  // Reasoning tokens are a subset of output tokens in the Runtime contract.
  // Keep them visible as a breakdown without charging them a second time.
  const totalTokens = input + output || usage?.total_tokens || 0;
  const cacheShare = input > 0
    ? `${Math.round(cached / input * 100)}% cache`
    : "";
  const turns = numberValue(usage?.turns) || (receipt ? 1 : 0);
  const turnText = `${turns} ${turns === 1 ? "turn" : "turns"}`;
  const toolText = `${toolCalls} ${toolCalls === 1 ? "tool" : "tools"}`;
  const totalTime = numberValue(latency?.total_ms) > 0
    ? `${formatDuration(numberValue(latency?.total_ms))} total`
    : "";
  const modelTime = numberValue(latency?.provider_ms) > 0
    ? `${formatDuration(numberValue(latency?.provider_ms))} model`
    : "";
  const toolTime = numberValue(latency?.tool_ms) > 0
    ? `${formatDuration(numberValue(latency?.tool_ms))} tools`
    : "";
  const ttft = latency?.first_token_ms === undefined
    ? ""
    : `${formatDuration(numberValue(latency.first_token_ms))} TTFT`;
  const timing = [totalTime, modelTime, toolTime].filter(Boolean).join(" · ");
  const tokenSummary = [
    totalTokens > 0 ? `${formatCompactCount(totalTokens)} tokens` : "",
    cacheShare
  ].filter(Boolean).join(" · ");
  const detailedValues = [
    turnText,
    toolText,
    totalTime,
    modelTime,
    toolTime,
    ttft,
    input > 0 ? `${input.toLocaleString()} in` : "",
    output > 0 ? `${output.toLocaleString()} out` : "",
    reasoning > 0 ? `${reasoning.toLocaleString()} reasoning` : "",
    cached > 0 ? `${cached.toLocaleString()} cached` : "",
    input > 0 ? `${(input - cached).toLocaleString()} uncached` : "",
    totalTokens > 0 ? `${totalTokens.toLocaleString()} tokens` : "",
    cacheShare
  ].filter(Boolean);
  const summary = [
    `${turnText} · ${toolText}`,
    timing,
    ttft,
    tokenSummary
  ].filter(Boolean).join(" | ");
  return (
    <div
      className="composerMeta"
      aria-label={`Run statistics: ${summary}`}
      title={detailedValues.join(" · ")}
    >
      <span>{summary}</span>
    </div>
  );
}

interface CatalogOption {
  value: string;
  label: string;
  disabled?: boolean;
}

function profileMutable(snapshot: RuntimeSnapshot, field: string): boolean {
  return snapshot.profile?.capabilities.mutable_fields.includes(field) ?? false;
}

function contextResourceLabel(
  resource: RuntimeSnapshot["contextResources"][number]
): string {
  if (resource.kind === "git_diff") {
    return resource.label ?? "Workspace diff";
  }
  if (resource.kind === "symbol" && resource.symbol) {
    return `${resource.symbol.name} · ${resource.path ?? "symbol"}:${
      (resource.range?.start.line ?? 0) + 1
    }`;
  }
  if (resource.kind === "diagnostics") {
    return `${resource.path ?? "Diagnostics"} · ${
      resource.diagnostics?.length ?? 0
    } diagnostics`;
  }
  if (resource.kind === "image") {
    return resource.label ?? resource.path ?? "Image";
  }
  if (resource.kind !== "selection" || !resource.range) {
    return resource.path ?? resource.label ?? resource.kind;
  }
  return `${resource.path}:${resource.range.start.line + 1}:` +
    `${resource.range.start.character + 1}-${resource.range.end.line + 1}:` +
    `${resource.range.end.character + 1}`;
}

function parseJSONObject(value: string): Record<string, unknown> | undefined {
  if (!value.trim()) return undefined;
  try {
    const parsed = JSON.parse(value) as unknown;
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    return undefined;
  }
  return undefined;
}

function approvalCommand(value: unknown): string {
  const data = typeof value === "string"
    ? parseJSONObject(value)
    : isObject(value) ? value : undefined;
  const command = data?.command ?? data?.cmd;
  return typeof command === "string" ? command : "";
}

function ResizeHandle({
  label,
  edge = "end",
  value,
  minimum,
  maximum,
  onDelta
}: {
  label: string;
  edge?: "start" | "end";
  value: number;
  minimum: number;
  maximum: number;
  onDelta: (delta: number) => void;
}) {
  const lastX = useRef(0);
  const pendingDelta = useRef(0);
  const frame = useRef<number>();
  const flush = () => {
    frame.current = undefined;
    if (pendingDelta.current === 0) return;
    const delta = pendingDelta.current;
    pendingDelta.current = 0;
    onDelta(delta);
  };
  return (
    <div
      className="resizeHandle"
      data-edge={edge}
      role="separator"
      aria-label={label}
      aria-orientation="vertical"
      aria-valuemin={minimum}
      aria-valuemax={maximum}
      aria-valuenow={value}
      tabIndex={0}
      onKeyDown={(event) => {
        if (event.key === "ArrowLeft") onDelta(-16);
        if (event.key === "ArrowRight") onDelta(16);
      }}
      onPointerDown={(event) => {
        lastX.current = event.clientX;
        event.currentTarget.setPointerCapture(event.pointerId);
      }}
      onPointerMove={(event) => {
        if (!event.currentTarget.hasPointerCapture(event.pointerId)) return;
        pendingDelta.current += event.clientX - lastX.current;
        lastX.current = event.clientX;
        if (frame.current === undefined) {
          frame.current = requestAnimationFrame(flush);
        }
      }}
      onPointerUp={(event) => {
        if (event.currentTarget.hasPointerCapture(event.pointerId)) {
          event.currentTarget.releasePointerCapture(event.pointerId);
        }
        if (frame.current !== undefined) cancelAnimationFrame(frame.current);
        flush();
      }}
    />
  );
}

function clamp(value: number, minimum: number, maximum: number): number {
  return Math.min(maximum, Math.max(minimum, value));
}

function BootState({title, detail, failed}: {title: string; detail?: string; failed?: boolean}) {
  return (
    <main className="bootState" data-failed={failed || undefined}>
      <div className="bootBrand">
        <CapybaraMark size="hero" />
        <span className="bootSignal">
          {failed
            ? <AlertTriangle size={14} />
            : <LoaderCircle className="spin" size={14} />}
        </span>
      </div>
      <h1>{title}</h1>
      {detail && <p>{detail}</p>}
    </main>
  );
}

export function projectTranscript(events: readonly RuntimeEvent[]): ConversationNode[] {
  const projection = projectConversation(events);
  return projection.order.flatMap((id) => {
    const node = projection.nodes.get(id);
    return node ? [node] : [];
  });
}

export function projectMessageChrome(
  events: readonly RuntimeEvent[]
): Map<string, MessageChrome> {
  const startedAt = new Map<string, number>();
  const receipts = new Map<string, Readonly<Record<string, unknown>>>();
  const completed = new Map<string, RuntimeEvent>();
  for (const event of events) {
    if (event.kind === "turn.started") {
      const timestamp = Date.parse(event.created_at);
      if (Number.isFinite(timestamp)) startedAt.set(event.turn_id, timestamp);
    } else if (event.kind === "turn.receipt") {
      receipts.set(event.turn_id, event.data);
    } else if (event.kind === "turn.completed") {
      completed.set(event.turn_id, event);
    }
  }
  const result = new Map<string, MessageChrome>();
  for (const [turnID, event] of completed) {
    const receipt = receipts.get(turnID);
    const latency = isObject(receipt?.latency) ? receipt.latency : undefined;
    const completedAt = Date.parse(event.created_at);
    const started = startedAt.get(turnID);
    const recordedTotal = optionalNumber(latency?.total_ms);
    const totalMS = recordedTotal ?? (
      started !== undefined && Number.isFinite(completedAt)
        ? Math.max(0, completedAt - started)
        : undefined
    );
    const firstTokenMS = optionalNumber(latency?.first_token_ms);
    const providerMS = optionalNumber(latency?.provider_ms);
    const outputTokens = optionalNumber(receipt?.output_tokens);
    const decodeMS = providerMS !== undefined && firstTokenMS !== undefined
      ? providerMS - firstTokenMS
      : undefined;
    const tokensPerSecond = decodeMS !== undefined && decodeMS > 0 &&
      outputTokens !== undefined && outputTokens > 0
      ? outputTokens / (decodeMS / 1_000)
      : undefined;
    result.set(turnID, {
      completedAt: event.created_at,
      ...(totalMS === undefined ? {} : {totalMS}),
      ...(firstTokenMS === undefined ? {} : {firstTokenMS}),
      ...(tokensPerSecond === undefined ? {} : {tokensPerSecond})
    });
  }
  return result;
}

export function latestContextAttribution(
  events: readonly RuntimeEvent[]
): ContextAttribution | undefined {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (event?.kind !== "usage" || !isObject(event.data.context)) continue;
    const context = event.data.context;
    return {
      estimatedTokens: numberValue(context.estimated_tokens),
      stableTokens:
        numberValue(context.stable_tokens) +
        numberValue(context.dynamic_tokens) +
        numberValue(context.continuation_tokens),
      toolTokens:
        numberValue(context.tool_definition_tokens) +
        numberValue(context.history_tool_tokens),
      messageTokens:
        numberValue(context.history_user_tokens) +
        numberValue(context.history_assistant_tokens) +
        numberValue(context.history_other_tokens),
      framingTokens: numberValue(context.provider_framing_tokens)
    };
  }
  return undefined;
}

function pendingRequestKey(sessionID: string, event?: RuntimeEvent): string {
  return `${sessionID}:${String(event?.data.request_id ?? "")}`;
}

function checkpointForTurn(
  checkpoints: readonly SessionCheckpoint[],
  turnID: string
): SessionCheckpoint | undefined {
  return checkpoints
    .filter((checkpoint) => checkpoint.turn_id === turnID)
    .reduce<SessionCheckpoint | undefined>(
      (latest, checkpoint) =>
        !latest || checkpoint.cursor > latest.cursor ? checkpoint : latest,
      undefined
    );
}

function composerSlashQuery(value: string): string | undefined {
  const match = /^\/([^\s]*)$/.exec(value);
  return match?.[1];
}

function pretty(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  return JSON.stringify(value, null, 2);
}

function isObject(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function optionalNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value >= 0
    ? value
    : undefined;
}

function formatDuration(milliseconds: number): string {
  return milliseconds < 1_000
    ? `${Math.round(milliseconds)} ms`
    : `${(milliseconds / 1_000).toFixed(milliseconds < 10_000 ? 2 : 1)} s`;
}

function formatCompactCount(value: number): string {
  return compactCountFormat.format(value);
}

function relativeTime(value: string): string {
  const delta = Date.now() - new Date(value).getTime();
  if (!Number.isFinite(delta) || delta < 60_000) return "now";
  if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m`;
  if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h`;
  return `${Math.floor(delta / 86_400_000)}d`;
}

function readThemeMode(): ThemeMode {
  const value = readPreference("ch.theme");
  return value === "light" || value === "dark" ? value : "system";
}

function applyThemeMode(theme: ThemeMode, systemDark: boolean) {
  writePreference("ch.theme", theme);
  const resolved = theme === "system"
    ? systemDark ? "dark" : "light"
    : theme;
  document.documentElement.dataset.theme = resolved;
  document.documentElement.style.colorScheme = resolved;
}
