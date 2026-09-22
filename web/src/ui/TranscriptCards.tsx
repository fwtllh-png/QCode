import {
  AlertTriangle,
  Atom,
  Bot,
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  FilePenLine,
  FileText,
  FolderSearch,
  LoaderCircle,
  MessageSquareText,
  Plus,
  ScanSearch,
  TerminalSquare,
  Wrench
} from "lucide-react";
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode
} from "react";
import type {
  ConversationNode,
  ProjectedEditPlanFile
} from "../projection/conversation";
import {Collapse} from "./primitives/Collapse";

type ReasoningNode = Extract<ConversationNode, {kind: "reasoning"}>;
type ToolNode = Extract<ConversationNode, {kind: "tool"}>;
type AgentNode = Extract<ConversationNode, {kind: "agent"}>;

const previewLineLimit = 16;
const terminalEscapeSequence = /(?:\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-_])/g;

export function ReasoningDisclosure({entry}: {entry: ReasoningNode}) {
  const [open, setOpen] = useState(false);
  const summaryRef = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    const summary = summaryRef.current;
    if (!summary) return;
    const frame = requestAnimationFrame(() => {
      summary.scrollLeft = entry.running
        ? summary.scrollWidth - summary.clientWidth
        : 0;
    });
    return () => cancelAnimationFrame(frame);
  }, [entry.running, entry.summary]);

  return (
    <div
      className="disclosure reasoningDisclosure"
      data-running={entry.running || undefined}
    >
      {entry.running && <span className="srOnly">Thinking</span>}
      <button onClick={() => setOpen((value) => !value)} aria-expanded={open}>
        <DisclosureLeading open={open} icon={<Atom size={14} />} />
        <span className="disclosureTitle">Think</span>
        <span className="disclosureSeparator" aria-hidden="true" />
        <small
          ref={summaryRef}
          data-follow-end={entry.running || undefined}
        >
          {entry.summary}
        </small>
      </button>
      <Collapse open={open}><div className="thinkBody">{entry.text}</div></Collapse>
    </div>
  );
}

export function AgentDisclosure({
  entry,
  onInspect
}: {
  entry: AgentNode;
  onInspect: (callID: string) => void;
}) {
  const [open, setOpen] = useState(
    entry.state === "running" || entry.state === "failed"
  );

  useEffect(() => {
    if (entry.state === "running" || entry.state === "failed") setOpen(true);
  }, [entry.state]);

  return (
    <div
      className="disclosure agentDisclosure"
      data-state={entry.state}
      data-agent-id={entry.agentID}
    >
      <button onClick={() => setOpen((value) => !value)} aria-expanded={open}>
        <DisclosureLeading open={open} icon={<Bot size={14} />} />
        <span className="disclosureTitle">
          {agentRoleLabel(entry.role)} · {entry.agentID}
        </span>
        <span className="disclosureSeparator" aria-hidden="true" />
        <small>
          {entry.summary}
          {entry.reasonCode ? ` · ${entry.reasonCode.replaceAll("_", " ")}` : ""}
          {entry.usage
            ? ` · in ${entry.usage.inputTokens} / out ${entry.usage.outputTokens}`
            : ""}
        </small>
        <span className="agentState" data-state={entry.state}>
          {entry.status.replaceAll("_", " ")}
        </span>
      </button>
      <Collapse open={open}>
        <ol className="agentActivity">
          {entry.activities.map((activity) => (
            <li
              key={activity.id}
              data-kind={activity.kind}
              data-state={activity.state}
            >
              <span className="agentActivityIcon" aria-hidden="true">
                {agentActivityIcon(activity.kind, activity.state)}
              </span>
              <span className="agentActivityText">
                <strong>{activity.title}</strong>
                {activity.summary && <small>{activity.summary}</small>}
              </span>
              {activity.callID && (
                <button
                  type="button"
                  className="agentInspect"
                  aria-label={`Inspect ${activity.title}`}
                  title={`Inspect ${activity.title}`}
                  onClick={() => onInspect(activity.callID ?? "")}
                >
                  <ScanSearch size={13} />
                </button>
              )}
            </li>
          ))}
        </ol>
      </Collapse>
    </div>
  );
}

function agentRoleLabel(role: string): string {
  const normalized = role.trim().replaceAll("_", " ");
  if (!normalized) return "Agent";
  return normalized.replace(/\b\w/g, (value) => value.toUpperCase());
}

function agentActivityIcon(
  kind: AgentNode["activities"][number]["kind"],
  state: AgentNode["activities"][number]["state"]
) {
  if (state === "failed") return <AlertTriangle size={13} />;
  if (state === "running") return <LoaderCircle className="spin" size={13} />;
  if (kind === "reasoning") return <Atom size={13} />;
  if (kind === "tool") return <Wrench size={13} />;
  if (kind === "message") return <MessageSquareText size={13} />;
  return <Check size={13} />;
}

export function ToolDisclosure({
  entry,
  onInspect,
  onAddContext
}: {
  entry: ToolNode;
  onInspect: (callID: string) => void;
  onAddContext: (callID: string, text: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const presentation = useMemo(() => toolPresentation(entry), [entry]);
  const expandable = Boolean(entry.output || entry.state === "failed");
  const toggle = () => {
    if (expandable) setOpen((value) => !value);
  };
  const toggleFromKeyboard = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!expandable || (event.key !== "Enter" && event.key !== " ")) return;
    event.preventDefault();
    toggle();
  };
  return (
    <div
      className="disclosure toolDisclosure"
      data-variant={entry.variant}
      data-failed={entry.state === "failed" || undefined}
      data-running={entry.state === "running" || undefined}
      data-call-id={entry.callID}
    >
      <div
        className="disclosureRow"
        role={expandable ? "button" : undefined}
        tabIndex={expandable ? 0 : undefined}
        aria-expanded={expandable ? open : undefined}
        aria-label={`${entry.title} ${entry.summary}`}
        onClick={toggle}
        onKeyDown={toggleFromKeyboard}
      >
        <DisclosureLeading open={open} icon={toolIcon(entry.variant)} />
        <span className="disclosureTitle">{entry.title}</span>
        <span className="disclosureSeparator" aria-hidden="true" />
        <small>{entry.summary}</small>
        {entry.state !== "completed" && (
          <span className="srOnly">{entry.state}</span>
        )}
      </div>
      <Collapse open={open}>
        <div className="toolExpanded">
          {entry.state === "failed" && entry.errorSummary && (
            <div className="toolIOCard">
              <section>
                <span>ERROR</span>
                <pre data-error>{entry.errorSummary}</pre>
              </section>
            </div>
          )}
          {renderToolBody(presentation, entry)}
          {entry.state === "failed" &&
            entry.output &&
            presentation.kind !== "shell" &&
            presentation.kind !== "generic" && (
              <div className="toolIOCard">
                <section>
                  <span>OUTPUT</span>
                  <pre>{entry.output}</pre>
                </section>
              </div>
            )}
          <div className="artifactActions toolActions">
            <button onClick={() => onInspect(entry.callID)}>
              <ScanSearch size={13} /> Inspect
            </button>
            {entry.contextText && (
              <button onClick={() => onAddContext(entry.callID, entry.contextText ?? "")}>
                <Plus size={13} /> Add output
              </button>
            )}
          </div>
        </div>
      </Collapse>
    </div>
  );
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

type ReadPresentation = {
  kind: "read";
  path: string;
  language: string;
  startLine: number;
  lines: string[];
  truncated: boolean;
};

type ShellPresentation = {
  kind: "shell";
  command: string;
  cwd: string;
  output: string;
  running: boolean;
  failed: boolean;
  exitCode?: number;
};

type SearchMatch = {line: number; text: string};
type SearchFile = {path: string; matches: SearchMatch[]};
type SearchPresentation =
  | {
      kind: "search-matches";
      files: SearchFile[];
      total: number;
      truncated: boolean;
    }
  | {
      kind: "search-paths";
      paths: string[];
      total: number;
      truncated: boolean;
    };

type SearchRenderRow =
  | {kind: "file"; path: string; count: number}
  | {kind: "match"; path: string; line: number; text: string}
  | {kind: "path"; path: string};

type GenericPresentation = {kind: "generic"};
type DiffPresentation = {
  kind: "diff";
  files: readonly ProjectedEditPlanFile[];
  changes: readonly Record<string, unknown>[];
  diff: string;
};
type ToolPresentation =
  | ReadPresentation
  | ShellPresentation
  | SearchPresentation
  | DiffPresentation
  | GenericPresentation;

function toolPresentation(entry: ToolNode): ToolPresentation {
  const args = recordValue(entry.arguments);
  if (entry.variant === "diff") {
    return {
      kind: "diff",
      files: entry.editPlan?.files ?? [],
      changes: entry.changes,
      diff: entry.editPlan?.diff ?? ""
    };
  }
  if (entry.variant === "read" && entry.tool !== "result_get") {
    const path = stringArgument(args, ["path", "file_path"]);
    if (path && !path.toLowerCase().endsWith(".pdf")) {
      return {
        kind: "read",
        path,
        language: languageForPath(path),
        startLine: Math.max(1, numberArgument(args, "start_line") || 1),
        lines: textLines(entry.output),
        truncated: entry.truncated
      };
    }
  }
  if (entry.variant === "shell") {
    return {
      kind: "shell",
      command: entry.command?.command ||
        stringArgument(args, ["command", "cmd"]) ||
        entry.summary,
      cwd: stringArgument(args, ["cwd", "workdir"]),
      output: stripTerminalEscapes(entry.output),
      running: entry.state === "running",
      failed: entry.state === "failed",
      exitCode: entry.command?.exitCode
    };
  }
  if (entry.variant === "search") {
    return searchPresentation(entry) ?? {kind: "generic"};
  }
  return {kind: "generic"};
}

function renderToolBody(
  presentation: ToolPresentation,
  entry: ToolNode
) {
  switch (presentation.kind) {
    case "read":
      return <ReadCard value={presentation} />;
    case "shell":
      return <TerminalCard value={presentation} />;
    case "search-matches":
    case "search-paths":
      return <SearchCard value={presentation} />;
    case "diff":
      return <DiffCard value={presentation} />;
    default:
      return (
        <div className="toolIOCard">
          <section>
            <span>IN</span>
            <pre>{pretty(entry.arguments)}</pre>
          </section>
          {entry.output && (
            <section>
              <span>OUT</span>
              <pre>{entry.output}</pre>
            </section>
          )}
        </div>
      );
  }
}

type DiffRow =
  | {kind: "path"; text: string; path: string}
  | {kind: "removed" | "added"; text: string};

function DiffCard({
  value
}: {
  value: DiffPresentation;
}) {
  const [expanded, setExpanded] = useState(false);
  const rows = useMemo<DiffRow[]>(() => {
    if (value.files.length > 0) {
      return value.files.flatMap((file) => [
        {kind: "path", text: file.path, path: file.path} as const,
        ...textLines(file.beforeExists ? file.before : "").map(
          (text): DiffRow => ({kind: "removed", text})
        ),
        ...textLines(file.afterExists ? file.after : "").map(
          (text): DiffRow => ({kind: "added", text})
        )
      ]);
    }
    return value.changes.flatMap((change) => {
      const path = stringArgument(change, ["path"]) || "workspace";
      const added = numberArgument(change, "added");
      const removed = numberArgument(change, "removed");
      return [{
        kind: "path",
        path,
        text: `${path}  +${added} -${removed}`
      } as const];
    });
  }, [value.changes, value.files]);
  const window = previewWindow(rows, expanded);
  const added = value.changes.reduce(
    (total, change) => total + numberArgument(change, "added"),
    0
  ) || value.files.reduce(
    (total, file) => total + textLines(file.afterExists ? file.after : "").length,
    0
  );
  const removed = value.changes.reduce(
    (total, change) => total + numberArgument(change, "removed"),
    0
  ) || value.files.reduce(
    (total, file) => total + textLines(file.beforeExists ? file.before : "").length,
    0
  );
  const paths = new Set([
    ...value.files.map((file) => file.path),
    ...value.changes.map((change) => stringArgument(change, ["path"])).filter(Boolean)
  ]);
  const copy = value.diff || rows.map((row) =>
    row.kind === "added"
      ? `+ ${row.text}`
      : row.kind === "removed"
        ? `- ${row.text}`
        : row.text
  ).join("\n");
  const renderRow = (row: DiffRow, key: string) => row.kind === "path" ? (
    <div className="diffPathLabel" key={key}>{row.text}</div>
  ) : (
    <div className="diffLine" data-line={row.kind} key={key}>{row.text || " "}</div>
  );
  return (
    <div className="toolSurface diffCard" data-diff>
      <div className="diffCopy"><CopyButton text={copy} /></div>
      <div className="diffBody">
        {window.head.map((row, index) => renderRow(row, `head-${index}`))}
        {window.hidden > 0 && (
          <ExpandRows
            expanded={expanded}
            hidden={window.hidden}
            noun="lines"
            onToggle={() => setExpanded((value) => !value)}
          />
        )}
        {window.tail.map((row, index) => renderRow(row, `tail-${index}`))}
      </div>
      <div className="diffFooter">
        +{added} -{removed} · {paths.size} {paths.size === 1 ? "file" : "files"}
      </div>
    </div>
  );
}

export function EditPlanPreview({
  files,
  diff
}: {
  files: readonly ProjectedEditPlanFile[];
  diff: string;
}) {
  return (
    <DiffCard
      value={{kind: "diff", files, diff, changes: []}}
    />
  );
}

function ReadCard({
  value
}: {
  value: ReadPresentation;
}) {
  const [expanded, setExpanded] = useState(false);
  const rows = value.lines.map((text, index) => ({
    number: value.startLine + index,
    text
  }));
  const window = previewWindow(rows, expanded);
  return (
    <div className="toolSurface readCard" data-read>
      <div className="toolSurfaceHeader">
        <span className="surfaceFileLabel">{value.path}</span>
        <span className="surfaceMeta">
          {value.truncated ? `${rows.length}+ lines` : `${rows.length} lines`}
        </span>
        <span className="surfaceMeta">{value.language}</span>
        <CopyButton text={value.lines.join("\n")} />
      </div>
      <div className="readBody">
        {window.head.map((row) => (
          <div className="readLine" key={row.number}>
            <span>{row.number}</span>
            <code>{row.text}</code>
          </div>
        ))}
        {window.hidden > 0 && (
          <ExpandRows
            expanded={expanded}
            hidden={window.hidden}
            noun="lines"
            onToggle={() => setExpanded((value) => !value)}
          />
        )}
        {window.tail.map((row) => (
          <div className="readLine" key={row.number}>
            <span>{row.number}</span>
            <code>{row.text}</code>
          </div>
        ))}
      </div>
    </div>
  );
}

function TerminalCard({value}: {value: ShellPresentation}) {
  const [expanded, setExpanded] = useState(false);
  const lines = textLines(value.output);
  const window = previewWindow(lines, expanded);
  const failed = value.failed;
  return (
    <div
      className="toolSurface terminalCard"
      data-terminal
      data-running={value.running || undefined}
      data-failed={failed || undefined}
    >
      <div className="terminalHeader">
        <span className="terminalStateDot" aria-hidden="true" />
        <span className="srOnly">
          {value.running ? "Command running" : failed ? "Command failed" : "Command finished"}
        </span>
        <span className="terminalCwd">{terminalDirectory(value.cwd)}</span>
        <code>{value.command}</code>
        {failed && value.exitCode !== undefined && (
          <span className="terminalExit">exit {value.exitCode}</span>
        )}
        {!value.running && value.output && <CopyButton text={value.output} />}
      </div>
      {!value.running && (
        <div className="terminalOutput">
          {lines.length === 0 ? (
            <div className="emptyToolOutput">No output</div>
          ) : (
            <>
              {window.head.map((line, index) => (
                <div className="terminalLine" key={`head-${index}`}>{line || " "}</div>
              ))}
              {window.hidden > 0 && (
                <ExpandRows
                  expanded={expanded}
                  hidden={window.hidden}
                  noun="lines"
                  onToggle={() => setExpanded((value) => !value)}
                />
              )}
              {window.tail.map((line, index) => (
                <div className="terminalLine" key={`tail-${index}`}>{line || " "}</div>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}

function SearchCard({
  value
}: {
  value: SearchPresentation;
}) {
  const [expanded, setExpanded] = useState(false);
  const rows: SearchRenderRow[] = value.kind === "search-paths"
    ? value.paths.map((path): SearchRenderRow => ({kind: "path", path}))
    : value.files.flatMap((file) => [
        {kind: "file", path: file.path, count: file.matches.length} as const,
        ...file.matches.map((match): SearchRenderRow => ({
          kind: "match",
          path: file.path,
          line: match.line,
          text: match.text
        }))
      ]);
  const window = previewWindow(rows, expanded);
  const shown = value.kind === "search-paths"
    ? value.paths.length
    : value.files.reduce((total, file) => total + file.matches.length, 0);
  const summary = value.kind === "search-paths"
    ? `${shown}${value.truncated ? ` / ${value.total}` : ""} paths`
    : `${shown}${value.truncated ? ` / ${value.total}` : ""} matches · ${value.files.length} files`;
  const copy = value.kind === "search-paths"
    ? value.paths.join("\n")
    : value.files.map((file) => [
        file.path,
        ...file.matches.map((match) => `${match.line}: ${match.text}`)
      ].join("\n")).join("\n\n");
  const renderRow = (
    row: SearchRenderRow,
    key: string
  ) => {
    if (row.kind === "match") {
      return (
        <div className="searchMatch" key={key}>
          <span>{row.line}:</span>
          <code>{row.text}</code>
        </div>
      );
    }
    return (
      <div className={row.kind === "file" ? "searchFileLabel" : "searchPathLabel"} key={key}>
        <span>{row.path}</span>
        {row.kind === "file" && <small>{row.count}</small>}
      </div>
    );
  };
  return (
    <div className="toolSurface searchCard" data-search={value.kind}>
      <div className="toolSurfaceHeader">
        <span>{summary}</span>
        {copy && <CopyButton text={copy} />}
      </div>
      <div className="searchBody">
        {window.head.map((row, index) => renderRow(row, `head-${index}`))}
        {window.hidden > 0 && (
          <ExpandRows
            expanded={expanded}
            hidden={window.hidden}
            noun="rows"
            onToggle={() => setExpanded((value) => !value)}
          />
        )}
        {window.tail.map((row, index) => renderRow(row, `tail-${index}`))}
      </div>
    </div>
  );
}

function CopyButton({text}: {text: string}) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    if (!text || copied) return;
    await navigator.clipboard.writeText(text);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1_000);
  };
  return (
    <button
      className="surfaceCopy"
      aria-label={copied ? "Copied" : "Copy"}
      onClick={() => void copy()}
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      <span>{copied ? "Copied" : "Copy"}</span>
    </button>
  );
}

function ExpandRows({
  expanded,
  hidden,
  noun,
  onToggle
}: {
  expanded: boolean;
  hidden: number;
  noun: string;
  onToggle: () => void;
}) {
  return (
    <button
      className="expandRows"
      aria-expanded={expanded}
      onClick={onToggle}
    >
      {expanded ? "Collapse" : `... ${hidden} more ${noun}`}
    </button>
  );
}

function previewWindow<T>(
  rows: readonly T[],
  expanded: boolean
): {head: readonly T[]; tail: readonly T[]; hidden: number} {
  if (expanded || rows.length <= previewLineLimit) {
    return {head: rows, tail: [], hidden: 0};
  }
  const headLength = Math.ceil(previewLineLimit / 2);
  const tailLength = previewLineLimit - headLength;
  return {
    head: rows.slice(0, headLength),
    tail: rows.slice(rows.length - tailLength),
    hidden: rows.length - previewLineLimit
  };
}

function searchPresentation(entry: ToolNode): SearchPresentation | undefined {
  const parsed = parseObject(entry.output);
  if (entry.tool === "file_list" && Array.isArray(parsed?.entries)) {
    const base = stringArgument(recordValue(entry.arguments), ["path"]);
    const paths = parsed.entries.flatMap((value) => {
      const item = recordValue(value);
      const path = stringArgument(item, ["path", "name"]);
      if (!path || item.type === "directory") return [];
      return [joinWorkspacePath(base, path)];
    });
    return {
      kind: "search-paths",
      paths,
      total: numberArgument(parsed, "total") || paths.length,
      truncated: Boolean(parsed.has_more) || entry.truncated
    };
  }
  if (Array.isArray(parsed?.matches)) {
    const pathMatches = parsed.matches.flatMap((value) => {
      const match = recordValue(value);
      const path = stringArgument(match, ["path"]);
      return path ? [path] : [];
    });
    if (pathMatches.length > 0) {
      return {
        kind: "search-paths",
        paths: pathMatches,
        total: numberArgument(parsed, "total") || pathMatches.length,
        truncated: Boolean(parsed.truncated) || entry.truncated
      };
    }
    const grouped = new Map<string, SearchMatch[]>();
    for (const value of parsed.matches) {
      const match = recordValue(value);
      const path = stringArgument(match, ["file"]);
      const line = numberArgument(match, "line");
      const text = stringArgument(match, ["text"]);
      if (!path || line < 1) continue;
      const current = grouped.get(path) ?? [];
      current.push({line, text});
      grouped.set(path, current);
    }
    if (grouped.size > 0 || parsed.matches.length === 0) {
      const files = [...grouped].map(([path, matches]) => ({path, matches}));
      return {
        kind: "search-matches",
        files,
        total: numberArgument(parsed, "total") ||
          files.reduce((total, file) => total + file.matches.length, 0),
        truncated: Boolean(parsed.truncated) || entry.truncated
      };
    }
  }
  if (entry.tool === "glob" || entry.tool === "search_files") {
    const paths = textLines(entry.output).filter(Boolean);
    return {
      kind: "search-paths",
      paths,
      total: paths.length,
      truncated: entry.truncated
    };
  }
  const grouped = new Map<string, SearchMatch[]>();
  for (const line of textLines(entry.output)) {
    const match = /^(.*):(\d+):(.*)$/.exec(line);
    if (!match) continue;
    const current = grouped.get(match[1]!) ?? [];
    current.push({line: Number(match[2]), text: match[3] ?? ""});
    grouped.set(match[1]!, current);
  }
  if (grouped.size === 0) return undefined;
  const files = [...grouped].map(([path, matches]) => ({path, matches}));
  return {
    kind: "search-matches",
    files,
    total: files.reduce((total, file) => total + file.matches.length, 0),
    truncated: entry.truncated
  };
}

function toolIcon(variant: ToolNode["variant"]): ReactNode {
  switch (variant) {
    case "read":
      return <FileText size={14} />;
    case "search":
      return <FolderSearch size={14} />;
    case "shell":
      return <TerminalSquare size={14} />;
    case "write":
    case "diff":
      return <FilePenLine size={14} />;
    default:
      return <TerminalSquare size={14} />;
  }
}

function languageForPath(path: string): string {
  const name = path.split(/[\\/]/).at(-1) ?? "";
  const extension = name.includes(".") ? name.split(".").at(-1) ?? "" : "";
  return extension.toLowerCase();
}

function terminalDirectory(value: string): string {
  if (!value || value === ".") return "$";
  return value.replace(/[\\/]+$/, "").split(/[\\/]/).at(-1) || value;
}

function joinWorkspacePath(base: string, path: string): string {
  if (!base || base === "." || path.startsWith(`${base}/`)) return path;
  return `${base.replace(/[\\/]+$/, "")}/${path.replace(/^[\\/]+/, "")}`;
}

function stripTerminalEscapes(value: string): string {
  return value.replace(terminalEscapeSequence, "");
}

function textLines(value: string): string[] {
  if (!value) return [];
  const lines = value.replace(/\r\n?/g, "\n").split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

function parseObject(value: string): Record<string, unknown> | undefined {
  try {
    return recordValue(JSON.parse(value));
  } catch {
    return undefined;
  }
}

function recordValue(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

function stringArgument(
  values: Record<string, unknown>,
  keys: readonly string[]
): string {
  for (const key of keys) {
    const value = values[key];
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function numberArgument(values: Record<string, unknown>, key: string): number {
  const value = values[key];
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function pretty(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}
