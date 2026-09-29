import {
  AlertTriangle,
  ArrowLeft,
  Download,
  FileCode2,
  FolderTree,
  GitCompareArrows,
  Image,
  Plus,
  RefreshCw,
  Search,
  TextSelect,
  X
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode
} from "react";
import {useModalFocus} from "./primitives/useModalFocus";
import {ScopeSlot} from "../runtime/requestScope";
import type {
  EditorRange,
  WorkspaceDiagnosticContext,
  WorkspaceDiff,
  WorkspaceEntry,
  WorkspaceImage,
  WorkspaceResource,
  WorkspaceSearchMatch,
  WorkspaceSymbol
} from "../protocol";
import type {RuntimeClient, RuntimeSnapshot} from "../runtime/client";
import "./WorkspaceContextDialog.css";

type ContextView = "files" | "changes" | "diagnostics";

interface Props {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  onClose: () => void;
  onError: (error: unknown) => void;
}

export function WorkspaceContextDialog({
  snapshot,
  client,
  onClose,
  onError
}: Props) {
  const [view, setView] = useState<ContextView>("files");
  const [query, setQuery] = useState("");
  const [matches, setMatches] = useState<readonly WorkspaceSearchMatch[]>([]);
  const [symbolQuery, setSymbolQuery] = useState("");
  const [symbols, setSymbols] = useState<readonly WorkspaceSymbol[]>([]);
  const [path, setPath] = useState(".");
  const [entries, setEntries] = useState<readonly WorkspaceEntry[]>([]);
  const [resource, setResource] = useState<WorkspaceResource>();
  const [selection, setSelection] = useState<EditorRange>();
  const [image, setImage] = useState<WorkspaceImage>();
  const [imageURL, setImageURL] = useState("");
  const [diagnostics, setDiagnostics] =
    useState<readonly WorkspaceDiagnosticContext[]>([]);
  const [diff, setDiff] = useState<WorkspaceDiff>();
  const [error, setError] = useState("");
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLElement>(null);
  const browsing = useRef(new ScopeSlot());
  const preview = useRef(new ScopeSlot());
  useModalFocus(dialogRef, true, onClose);
  const reportError = useCallback((value: unknown) => {
    setError(value instanceof Error ? value.message : String(value));
    onError(value);
  }, [onError]);
  const selected = snapshot.sessions.find(
    (session) => session.session_id === snapshot.selectedSessionID
  );

  useEffect(() => {
    const scope = browsing.current.renew();
    void client.browseWorkspace(".").then((result) => {
      if (!scope.live) return;
      setPath(result.path);
      setEntries(result.entries);
    }, reportError);
  }, [client, reportError]);

  useEffect(() => () => {
    browsing.current.renew();
    preview.current.renew();
  }, []);

  useEffect(() => () => {
    if (imageURL) URL.revokeObjectURL(imageURL);
  }, [imageURL]);

  const browse = (nextPath: string) => {
    const scope = browsing.current.renew();
    void client.browseWorkspace(nextPath).then((result) => {
      if (!scope.live) return;
      setPath(result.path);
      setEntries(result.entries);
    }, reportError);
  };
  const openPath = async (target: string) => {
    const scope = preview.current.renew();
    try {
      if (isWorkspaceImagePath(target)) {
        const next = await client.readWorkspaceImage(target);
        if (!scope.live) return;
        const blob = await client.downloadWorkspaceContent(next.content_handle);
        if (!scope.live) return;
        if (imageURL) URL.revokeObjectURL(imageURL);
        setResource(undefined);
        setSelection(undefined);
        setImage(next);
        setImageURL(URL.createObjectURL(blob));
        return;
      }
      const next = await client.readWorkspaceResource(target);
      if (!scope.live) return;
      if (imageURL) URL.revokeObjectURL(imageURL);
      setImage(undefined);
      setImageURL("");
      setSelection(undefined);
      setResource(next);
    } catch (error) {
      if (scope.live) reportError(error);
    }
  };
  const download = async (
    value: Pick<WorkspaceResource | WorkspaceImage, "content_handle" | "path">
  ) => {
    try {
      const blob = await client.downloadWorkspaceContent(value.content_handle);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = value.path.split("/").at(-1) || "download";
      anchor.click();
      URL.revokeObjectURL(url);
    } catch (error) {
      reportError(error);
    }
  };
  const refreshDiff = () => void client.workspaceDiff().then(setDiff, reportError);
  const refreshDiagnostics = () => void client.workspaceDiagnostics().then(
    (result) => setDiagnostics(result.diagnostics),
    reportError
  );

  return (
    <div className="contextDialogOverlay" data-motion-backdrop role="presentation" onMouseDown={(event) => {
      if (event.target === event.currentTarget) onClose();
    }}>
      <section
        ref={dialogRef}
        className="contextDialog"
        data-motion-surface
        role="dialog"
        aria-modal="true"
        aria-labelledby="context-dialog-title"
      >
        <header className="contextDialogHeader">
          <div>
            <h2 id="context-dialog-title">Add context</h2>
            <p>{snapshot.workspaceRoot}</p>
          </div>
          <button
            ref={closeRef}
            type="button"
            aria-label="Close context browser"
            onClick={onClose}
          >
            <X size={18} />
          </button>
        </header>
        <nav className="contextDialogTabs" aria-label="Context sources">
          <button
            type="button"
            aria-current={view === "files" ? "page" : undefined}
            onClick={() => setView("files")}
          >
            <FileCode2 size={14} /> Files
          </button>
          <button
            type="button"
            aria-current={view === "changes" ? "page" : undefined}
            onClick={() => {
              setView("changes");
              refreshDiff();
            }}
          >
            <GitCompareArrows size={14} /> Changes
            <small>{selected?.changed_files ?? 0}</small>
          </button>
          <button
            type="button"
            aria-current={view === "diagnostics" ? "page" : undefined}
            onClick={() => {
              setView("diagnostics");
              refreshDiagnostics();
            }}
          >
            <AlertTriangle size={14} /> Diagnostics
          </button>
        </nav>
        {error && <div className="contextDialogError" role="alert">{error}</div>}
        <div className="contextDialogBody">
          {view === "files" && (
            <FileContextView
              client={client}
              snapshot={snapshot}
              path={path}
              entries={entries}
              query={query}
              matches={matches}
              symbolQuery={symbolQuery}
              symbols={symbols}
              resource={resource}
              selection={selection}
              image={image}
              imageURL={imageURL}
              onPath={browse}
              onOpenPath={(value) => void openPath(value)}
              onQueryChange={setQuery}
              onMatches={setMatches}
              onSymbolQueryChange={setSymbolQuery}
              onSymbols={setSymbols}
              onSelection={setSelection}
              onDownload={(value) => void download(value)}
              onError={reportError}
            />
          )}
          {view === "changes" && (
            <ChangesContextView
              snapshot={snapshot}
              client={client}
              diff={diff}
              onRefresh={refreshDiff}
              onError={reportError}
            />
          )}
          {view === "diagnostics" && (
            <DiagnosticsContextView
              client={client}
              diagnostics={diagnostics}
              onRefresh={refreshDiagnostics}
            />
          )}
        </div>
      </section>
    </div>
  );
}

function FileContextView({
  client,
  snapshot,
  path,
  entries,
  query,
  matches,
  symbolQuery,
  symbols,
  resource,
  selection,
  image,
  imageURL,
  onPath,
  onOpenPath,
  onQueryChange,
  onMatches,
  onSymbolQueryChange,
  onSymbols,
  onSelection,
  onDownload,
  onError
}: {
  client: RuntimeClient;
  snapshot: RuntimeSnapshot;
  path: string;
  entries: readonly WorkspaceEntry[];
  query: string;
  matches: readonly WorkspaceSearchMatch[];
  symbolQuery: string;
  symbols: readonly WorkspaceSymbol[];
  resource?: WorkspaceResource;
  selection?: EditorRange;
  image?: WorkspaceImage;
  imageURL: string;
  onPath: (path: string) => void;
  onOpenPath: (path: string) => void;
  onQueryChange: (value: string) => void;
  onMatches: (values: readonly WorkspaceSearchMatch[]) => void;
  onSymbolQueryChange: (value: string) => void;
  onSymbols: (values: readonly WorkspaceSymbol[]) => void;
  onSelection: (value?: EditorRange) => void;
  onDownload: (
    value: Pick<WorkspaceResource | WorkspaceImage, "content_handle" | "path">
  ) => void;
  onError: (error: unknown) => void;
}) {
  return (
    <div className="contextFileLayout">
      <div className="contextBrowser">
        <form onSubmit={(event) => {
          event.preventDefault();
          if (!query.trim()) return;
          void client.searchWorkspace(query).then(
            (result) => onMatches(result.matches),
            onError
          );
        }}>
          <Search size={14} />
          <input
            aria-label="Search workspace"
            placeholder="Search files"
            value={query}
            onChange={(event) => onQueryChange(event.target.value)}
          />
        </form>
        <form onSubmit={(event) => {
          event.preventDefault();
          if (!symbolQuery.trim()) return;
          void client.searchWorkspaceSymbols(symbolQuery).then(
            (result) => onSymbols(result.symbols),
            onError
          );
        }}>
          <TextSelect size={14} />
          <input
            aria-label="Search workspace symbols"
            placeholder="Search symbols"
            value={symbolQuery}
            onChange={(event) => onSymbolQueryChange(event.target.value)}
          />
        </form>
        <div className="contextPath">
          <FolderTree size={14} />
          <span>{path}</span>
        </div>
        <div className="contextResults">
          {path !== "." && (
            <button
              type="button"
              onClick={() => onPath(path.split("/").slice(0, -1).join("/") || ".")}
            >
              <ArrowLeft size={13} /><span>Parent directory</span>
            </button>
          )}
          {entries.map((entry) => (
            <button
              type="button"
              key={entry.path}
              onClick={() => entry.kind === "directory"
                ? onPath(entry.path)
                : onOpenPath(entry.path)}
            >
              {entry.kind === "directory"
                ? <FolderTree size={13} />
                : isWorkspaceImagePath(entry.path)
                  ? <Image size={13} />
                  : <FileCode2 size={13} />}
              <span>{entry.path}</span>
              <small>{entry.kind}</small>
            </button>
          ))}
          {matches.map((match) => (
            <button
              type="button"
              key={`${match.path}:${match.line}:${match.column}`}
              onClick={() => onOpenPath(match.path)}
            >
              <Search size={13} />
              <span>{match.path}:{match.line}</span>
              <small>{match.preview}</small>
            </button>
          ))}
          {symbols.map((symbol) => (
            <button
              type="button"
              key={`${symbol.path}:${symbol.line}:${symbol.name}`}
              onClick={() => client.addSymbolContext(symbol)}
            >
              <TextSelect size={13} />
              <span>{symbol.name}</span>
              <small>{symbol.kind} · {symbol.path}:{symbol.line}</small>
            </button>
          ))}
        </div>
      </div>
      <div className="contextPreview">
        {resource ? (
          <>
            <ContextPreviewHeader
              title={resource.path}
              actions={
                <>
                  <button
                    type="button"
                    disabled={snapshot.contextResources.some((value) =>
                      value.path === resource.path && value.kind === "file"
                    )}
                    onClick={() => client.addWorkspaceContext(resource)}
                  >
                    <Plus size={13} /> Add file
                  </button>
                  <button
                    type="button"
                    disabled={!selection}
                    onClick={() => {
                      if (selection) client.addWorkspaceContext(resource, selection);
                    }}
                  >
                    <TextSelect size={13} /> Add selection
                  </button>
                  <button
                    type="button"
                    aria-label="Download resource"
                    onClick={() => onDownload(resource)}
                  >
                    <Download size={13} />
                  </button>
                </>
              }
            />
            <textarea
              aria-label="Workspace resource content"
              readOnly
              spellCheck={false}
              value={resource.content}
              onSelect={(event) => onSelection(selectionRange(
                event.currentTarget.value,
                event.currentTarget.selectionStart,
                event.currentTarget.selectionEnd
              ))}
            />
          </>
        ) : image && imageURL ? (
          <>
            <ContextPreviewHeader
              title={image.path}
              actions={
                <>
                  <button
                    type="button"
                    disabled={snapshot.contextResources.some((value) =>
                      value.path === image.path && value.kind === "image"
                    )}
                    onClick={() => client.addImageContext(image)}
                  >
                    <Plus size={13} /> Add image
                  </button>
                  <button
                    type="button"
                    aria-label="Download image"
                    onClick={() => onDownload(image)}
                  >
                    <Download size={13} />
                  </button>
                </>
              }
            />
            <img src={imageURL} alt={image.label} />
          </>
        ) : (
          <div className="contextPreviewEmpty">
            <FileCode2 size={24} />
            <strong>Select a file</strong>
            <span>Preview it before adding the file or a precise selection.</span>
          </div>
        )}
      </div>
    </div>
  );
}

function ChangesContextView({
  snapshot,
  client,
  diff,
  onRefresh,
  onError
}: {
  snapshot: RuntimeSnapshot;
  client: RuntimeClient;
  diff?: WorkspaceDiff;
  onRefresh: () => void;
  onError: (error: unknown) => void;
}) {
  const selected = snapshot.sessions.find(
    (session) => session.session_id === snapshot.selectedSessionID
  );
  return (
    <div className="contextSingleView">
      <div className="contextActionBar">
        <span>
          <strong>Workspace changes</strong>
          <small>{selected?.changed_files ?? 0} changed files</small>
        </span>
        <button type="button" onClick={onRefresh}>
          <RefreshCw size={13} /> Refresh
        </button>
        {diff?.diff && (
          <button type="button" onClick={() => client.addGitDiffContext(diff)}>
            <Plus size={13} /> Add diff
          </button>
        )}
      </div>
      {diff?.diff ? (
        <UnifiedDiff text={diff.diff} />
      ) : (
        <div className="contextPreviewEmpty">
          <GitCompareArrows size={24} />
          <strong>No workspace changes</strong>
          <span>The current workspace has no diff to add.</span>
        </div>
      )}
      {selected?.isolation === "worktree" && (
        <div className="mergeActions">
          <button type="button" onClick={() => void client.previewMerge().catch(onError)}>
            Preview merge
          </button>
          <button
            type="button"
            disabled={!snapshot.mergePlan}
            onClick={() => void client.applyMerge().catch(onError)}
          >
            Apply merge
          </button>
        </div>
      )}
      {snapshot.mergePlan?.diff && <UnifiedDiff text={snapshot.mergePlan.diff} />}
    </div>
  );
}

function DiagnosticsContextView({
  client,
  diagnostics,
  onRefresh
}: {
  client: RuntimeClient;
  diagnostics: readonly WorkspaceDiagnosticContext[];
  onRefresh: () => void;
}) {
  return (
    <div className="contextSingleView">
      <div className="contextActionBar">
        <span>
          <strong>Diagnostics</strong>
          <small>{diagnostics.length} tool results with diagnostics</small>
        </span>
        <button type="button" onClick={onRefresh}>
          <RefreshCw size={13} /> Refresh
        </button>
      </div>
      {diagnostics.length === 0 ? (
        <div className="contextPreviewEmpty">
          <AlertTriangle size={24} />
          <strong>No diagnostics recorded</strong>
          <span>Run an analysis tool to collect compiler or language-server findings.</span>
        </div>
      ) : (
        <div className="diagnosticResults">
          {diagnostics.map((diagnostic) => (
            <button
              type="button"
              key={`${diagnostic.call_id}:${diagnostic.context.path}`}
              onClick={() => client.addDiagnosticsContext(diagnostic)}
            >
              <AlertTriangle size={14} />
              <span>
                <strong>{diagnostic.context.path}</strong>
                <small>
                  {diagnostic.status} · {diagnostic.context.diagnostics?.length ?? 0} findings
                </small>
              </span>
              <Plus size={13} />
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function ContextPreviewHeader({
  title,
  actions
}: {
  title: string;
  actions: ReactNode;
}) {
  return (
    <header>
      <strong title={title}>{title}</strong>
      <div>{actions}</div>
    </header>
  );
}

function UnifiedDiff({text}: {text: string}) {
  return (
    <pre className="contextDiff" aria-label="Workspace diff">
      {text.split("\n").map((line, index) => (
        <span
          key={`${index}:${line}`}
          data-line={line.startsWith("+") && !line.startsWith("+++")
            ? "added"
            : line.startsWith("-") && !line.startsWith("---")
              ? "removed"
              : line.startsWith("@@")
                ? "hunk"
                : undefined}
        >
          {line || " "}
        </span>
      ))}
    </pre>
  );
}

export function selectionRange(
  content: string,
  startOffset: number,
  endOffset: number
): EditorRange | undefined {
  const start = Math.max(0, Math.min(startOffset, content.length));
  const end = Math.max(start, Math.min(endOffset, content.length));
  if (start === end) return undefined;
  return {
    start: positionAt(content, start),
    end: positionAt(content, end)
  };
}

function positionAt(content: string, offset: number) {
  const prefix = content.slice(0, offset);
  const lastNewline = prefix.lastIndexOf("\n");
  return {
    line: prefix.split("\n").length - 1,
    character: lastNewline < 0 ? prefix.length : prefix.length - lastNewline - 1
  };
}

function isWorkspaceImagePath(path: string): boolean {
  return /\.(?:png|jpe?g|gif|webp)$/i.test(path);
}
