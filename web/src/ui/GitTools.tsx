import {
  ArrowLeft, Check, ChevronDown, FileDiff, GitBranch, GitCommitHorizontal,
  LoaderCircle, Maximize2, Minimize2, PanelLeftClose, PanelLeftOpen,
  Plus, RefreshCw, Search, Upload, X
} from "lucide-react";
import {useCallback, useEffect, useMemo, useRef, useState} from "react";
import type {GitActionRequest, GitChange, GitOverview, GitPatch, SessionSummary, WorkspaceDescriptor} from "../protocol";
import type {RuntimeClient} from "../runtime/client";
import {GitPatchView} from "./GitPatchView";
import {IconButton} from "./primitives/IconButton";
import {Presence, usePresenceActive} from "./primitives/Presence";
import {Skeleton} from "./primitives/Skeleton";
import {useModalFocus} from "./primitives/useModalFocus";

export function gitTotals(files: readonly GitChange[], staged?: boolean) {
  return files.reduce((total, file) => {
    const stats = staged === undefined ? [file.staged, file.unstaged] : [staged ? file.staged : file.unstaged];
    for (const stat of stats) {
      total.added += stat?.added ?? 0;
      total.removed += stat?.removed ?? 0;
    }
    return total;
  }, {added: 0, removed: 0});
}

function Counts({files, staged}: {files: readonly GitChange[]; staged?: boolean}) {
  const totals = gitTotals(files, staged);
  const partial = staged !== true && files.some((file) => file.untracked && !file.unstaged);
  return <span className="gitCounts"
    title={partial ? "Partial totals: some new files have unavailable line counts." : undefined}
    aria-label={`${totals.added} additions, ${totals.removed} deletions${partial ? " (partial)" : ""}`}>
    <span>+{totals.added}</span><span>-{totals.removed}</span>
  </span>;
}

function hasChange(file: GitChange, staged: boolean) {
  return staged ? file.index !== " " && !file.untracked : file.worktree !== " ";
}

export function GitTools({client, workspace, session, busy, mutationDisabled, modal: compactViewport, onClose}: {
  client: RuntimeClient;
  workspace: WorkspaceDescriptor;
  session?: SessionSummary;
  busy: boolean;
  mutationDisabled: string;
  modal: boolean;
  onClose: () => void;
}) {
  const active = usePresenceActive();
  const ref = useRef<HTMLElement>(null);
  const [expanded, setExpanded] = useState(false);
  const [view, setView] = useState<"overview" | "changes" | "branches">("overview");
  const [maximized, setMaximized] = useState(false);
  const [filesVisible, setFilesVisible] = useState(true);
  const fullscreen = view === "changes" && maximized;
  const modal = fullscreen || (compactViewport && (expanded || view !== "overview"));
  const [data, setData] = useState<GitOverview>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState(false);
  const [commitOpen, setCommitOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const [newBranch, setNewBranch] = useState("");
  const [staged, setStaged] = useState(false);
  const [path, setPath] = useState("");
  const [patch, setPatch] = useState<GitPatch>();
  const [patchError, setPatchError] = useState("");
  const request = useRef<AbortController>();
  const live = useRef(active);
  live.current = active;
  useModalFocus(ref, modal, onClose);
  useEffect(() => {
    live.current = active;
    return () => { live.current = false; };
  }, [active]);

  const refresh = useCallback(async () => {
    request.current?.abort();
    if (!live.current) return;
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    setError("");
    try {
      const value = await client.gitOverview(workspace.id, controller.signal);
      if (!controller.signal.aborted) setData(value);
    } catch (reason) {
      if (!controller.signal.aborted) {
        setData(undefined);
        setError(reason instanceof Error ? reason.message : "Git query failed");
      }
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [client, workspace.id]);

  useEffect(() => {
    if (active) void refresh();
    return () => request.current?.abort();
  }, [active, refresh, busy]);

  useEffect(() => {
    if (!active) return;
    const focused = () => { if (!request.current || !loading) void refresh(); };
    window.addEventListener("focus", focused);
    return () => window.removeEventListener("focus", focused);
  }, [active, refresh, loading]);

  useEffect(() => {
    if (!active || modal || commitOpen) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented ||
          !ref.current?.contains(document.activeElement) ||
          document.querySelector('[aria-modal="true"]:not([aria-hidden])')) return;
      event.preventDefault();
      if (view !== "overview") setView("overview");
      else onClose();
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => document.removeEventListener("keydown", closeOnEscape);
  }, [active, modal, commitOpen, view, onClose]);

  const files = useMemo(() => data?.files.filter((file) => hasChange(file, staged)) ?? [], [data, staged]);
  const selectedFile = files.find((file) => file.path === path);
  useEffect(() => {
    setPatch(undefined);
    setPatchError("");
    if (!active || view !== "changes" || !selectedFile || loading) return;
    const controller = new AbortController();
    void client.gitPatch(workspace.id, selectedFile.path, staged, controller.signal).then(
      (value) => { if (!controller.signal.aborted) setPatch(value); },
      (reason) => { if (!controller.signal.aborted) setPatchError(reason instanceof Error ? reason.message : "Diff query failed"); }
    );
    return () => controller.abort();
  }, [active, view, selectedFile, staged, loading, client, workspace.id]);

  async function submit(action: GitActionRequest) {
    if (mutationDisabled || pending) return;
    setPending(true);
    setError("");
    try {
      const result = await client.executeGitAction(workspace.id, session?.session_id, action);
      if (!live.current) return;
      await refresh();
      if (!live.current) return;
      if (!result.problem || result.completed.includes("git_commit")) {
        setCommitOpen(false);
        setView("overview");
      }
      const messages: string[] = [];
      if (result.commit_hash) messages.push(`Committed ${result.commit_hash}.`);
      if (result.completed.includes("git_push")) messages.push(`Pushed to ${action.remote}/${action.branch}.`);
      if (result.completed.includes("git_switch")) messages.push(`Switched to ${action.new_branch}.`);
      if (result.completed.includes("git_add") && !result.completed.includes("git_commit")) messages.push("Files staged. Check HEAD before retrying the commit.");
      setNotice(messages.join(" "));
      if (result.problem) setError(result.problem.message);
    } catch (reason) {
      if (live.current) setError(reason instanceof Error ? reason.message : "Git request failed");
    } finally {
      if (live.current) setPending(false);
    }
  }

  async function switchBranch(branch: string) {
    if (pending || busy || branch === data?.branch) return;
    setPending(true);
    setError("");
    try {
      await client.switchWorkspaceBranch(workspace.id, branch);
      if (live.current) {
        setView("overview");
        await refresh();
      }
    } catch (reason) {
      if (live.current) setError(`Branch not switched. ${reason instanceof Error ? reason.message : "Git operation failed."}`);
    } finally {
      if (live.current) setPending(false);
    }
  }

  const disabled = Boolean(mutationDisabled || pending || loading || !data?.repository || !data.root || data.detached || data.files.some((file) => file.conflict));
  return <>
    <aside
      ref={ref} className="gitTools" id="git-tools" data-motion-surface
      data-expanded={expanded || view === "changes" || undefined}
      data-view={view}
      data-fullscreen={fullscreen || undefined}
      data-modal={modal || undefined}
      role={modal ? "dialog" : "complementary"} aria-modal={modal || undefined}
      aria-label="Git tools" tabIndex={-1}
      {...(commitOpen ? {inert: "", "aria-hidden": true} : {})}
    >
      <header>
        {view !== "overview" && <IconButton label="Back to Git tools" icon={<ArrowLeft size={16} />}
          onClick={() => {setView("overview"); setMaximized(false);}} />}
        <h2>{view === "changes" ? "Changes" : view === "branches" ? "Branches" : "Git tools"}</h2>
        <IconButton label="Refresh Git" icon={loading ? <LoaderCircle className="spin" size={15} /> : <RefreshCw size={15} />}
          disabled={loading || pending} onClick={() => void refresh()} />
        {view !== "changes" && <IconButton label={expanded ? "Compact Git tools" : "Expand Git tools"}
          icon={expanded ? <Minimize2 size={15} /> : <Maximize2 size={15} />} onClick={() => setExpanded(!expanded)} />}
        {view === "changes" && <IconButton label={maximized ? "Restore diff viewer" : "Expand diff viewer"}
          icon={maximized ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
          expanded={maximized} onClick={() => setMaximized(!maximized)} />}
        <IconButton label="Close Git tools" icon={<X size={16} />} onClick={onClose} />
      </header>
      <div className="gitScope" title={workspace.root}><span>{workspace.label}</span><small>Workspace</small></div>
      {error && <p className="gitProblem" role="alert">{error}</p>}
      {notice && <p className="gitNotice" role="status">{notice}</p>}
      {!data && loading ? <Skeleton label="Loading Git status" /> : data && !data.repository
        ? <p className="gitEmpty">This Workspace is not a Git repository.</p>
        : data && <>
          {view === "overview" && <div className="gitSummary">
            <button onClick={() => setView("changes")} className="gitSummaryRow">
              <FileDiff size={18} /><span>Changes <small>{data.files.length} files</small></span><Counts files={data.files} />
            </button>
            <button onClick={() => setView("branches")} className="gitSummaryRow">
              <GitBranch size={18} /><span title={data.branch}>{data.branch || "No branch"}{data.detached && <small>Detached HEAD</small>}</span><ChevronDown size={16} />
            </button>
            <button className="gitSummaryRow" disabled={disabled} title={mutationDisabled || undefined}
              onClick={() => setCommitOpen(true)}>
              <GitCommitHorizontal size={18} /><span>Commit or push</span>
            </button>
            {mutationDisabled && <p className="gitEmpty">{mutationDisabled}</p>}
            {!data.root && <p className="gitEmpty">Open the Git repository root to commit or push.</p>}
            {data.files.some((file) => file.conflict) && <p className="gitProblem">Resolve merge conflicts before committing.</p>}
          </div>}
          {data.files.some((file) => file.untracked && !file.unstaged) && <p className="gitEmpty">
            New files with unavailable line counts: {data.files.filter((file) => file.untracked && !file.unstaged).length} (excluded from totals).
          </p>}
          {view === "branches" && <div className="gitBranchView">
            <label className="gitSearch"><Search size={15} /><input autoFocus type="search" aria-label="Search Git branches"
              value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="Search branches" /></label>
            <div className="gitBranchList">
              {(data.branches ?? []).filter((branch) => branch.toLowerCase().includes(filter.toLowerCase())).map((branch) =>
                <button key={branch} title={branch} aria-current={branch === data.branch ? "true" : undefined}
                  disabled={busy || pending || loading} onClick={() => void switchBranch(branch)}>
                  <GitBranch size={16} /><span>{branch}</span>{branch === data.branch && <Check size={15} />}
                </button>)}
              {!(data.branches ?? []).some((branch) => branch.toLowerCase().includes(filter.toLowerCase())) &&
                <p className="gitEmpty">No matching local branches.</p>}
            </div>
            {busy && <p className="gitEmpty">Finish active work before switching branches.</p>}
            <form className="gitCreateBranch" onSubmit={(event) => {
              event.preventDefault();
              void submit({action: "create_branch", branch: data.branch ?? "", revision: data.revision, new_branch: newBranch.trim()});
            }}>
              <input aria-label="New Git branch name" placeholder="New branch name" value={newBranch}
                onChange={(event) => setNewBranch(event.target.value)} maxLength={255} />
              <button type="submit" disabled={disabled || !newBranch.trim() || newBranch.startsWith("-")}
                title={mutationDisabled || "Create and switch branch"}><Plus size={15} />Create</button>
            </form>
          </div>}
          {view === "changes" && <>
            <div className="gitChangeToolbar">
              <IconButton label={filesVisible ? "Hide changed files" : "Show changed files"}
                icon={filesVisible ? <PanelLeftClose size={16} /> : <PanelLeftOpen size={16} />}
                expanded={filesVisible} controls="git-changed-files"
                onClick={() => setFilesVisible(!filesVisible)} />
              <select aria-label="Git change scope" value={staged ? "staged" : "unstaged"} onChange={(event) => {setStaged(event.target.value === "staged"); setPath("");}}>
                <option value="unstaged">Unstaged</option><option value="staged">Staged</option>
              </select>
              <small>{files.length} files</small><Counts files={files} staged={staged} />
            </div>
            <div className="gitReview" data-files-hidden={!filesVisible || undefined}>
              <div className="gitFileList" id="git-changed-files" hidden={!filesVisible}>
                {files.length === 0 && <p className="gitEmpty">No {staged ? "staged" : "unstaged"} changes.</p>}
                {files.map((file) => <button key={file.path} title={file.path}
                  aria-current={path === file.path ? "true" : undefined} onClick={() => setPath(file.path)}>
                  <span className="gitFileStatus" data-conflict={file.conflict || undefined}>{file.conflict ? "!" : file.untracked ? "?" : staged ? file.index : file.worktree}</span>
                  <span>{file.path}</span>
                  {file.untracked && <small>New</small>}
                  {(staged ? file.staged : file.unstaged)?.binary ? <small>Binary</small>
                    : file.untracked && !file.unstaged ? <small>Lines unavailable</small>
                    : <Counts files={[file]} staged={staged} />}
                </button>)}
              </div>
              <section className="gitPatch" aria-label="Git file diff" aria-busy={Boolean(selectedFile && !patch && !patchError)}>
                {!selectedFile ? <p className="gitEmpty">No file selected.</p> : <>
                  <h3 title={path}>{path}</h3>
                  {patchError ? <p className="gitProblem" role="alert">{patchError}</p> : patch
                    ? patch.diff ? <GitPatchView key={`${staged}:${path}`} diff={patch.diff} /> : <p className="gitEmpty">No textual diff.</p>
                    : <Skeleton label="Loading Git diff" />}
                </>}
              </section>
            </div>
          </>}
        </>}
    </aside>
    <Presence open={commitOpen} kind="dialog">
      {data && <CommitDialog data={data} disabled={disabled} pending={pending} error={error}
        onClose={() => setCommitOpen(false)} onSubmit={submit} />}
    </Presence>
  </>;
}

function CommitDialog({data, disabled, pending, error, onClose, onSubmit}: {
  data: GitOverview;
  disabled: boolean;
  pending: boolean;
  error: string;
  onClose: () => void;
  onSubmit: (request: GitActionRequest) => Promise<void>;
}) {
  const ref = useRef<HTMLElement>(null);
  useModalFocus(ref, true, () => { if (!pending) onClose(); });
  const [message, setMessage] = useState("");
  const [includeUnstaged, setIncludeUnstaged] = useState(false);
  const [remote, setRemote] = useState(data.remotes.length === 1 ? data.remotes[0] : "");
  const files = data.files.filter((file) => includeUnstaged || hasChange(file, true));
  const request = (action: GitActionRequest["action"]) => void onSubmit({
    action, branch: data.branch ?? "", revision: data.revision, message,
    include_unstaged: includeUnstaged, paths: files.map((file) => file.path),
    remote: action === "push" || action === "commit_push" ? remote : undefined
  });
  return <div className="gitCommitOverlay" data-motion-backdrop onMouseDown={(event) => {
    if (event.target === event.currentTarget && !pending) onClose();
  }}>
    <section ref={ref} className="gitCommitDialog" role="dialog" aria-modal="true" aria-label="Commit or push" data-motion-surface tabIndex={-1}>
      <header><GitBranch size={16} /><span title={data.branch}>{data.branch}</span><Counts files={files} />
        <IconButton label="Close commit dialog" icon={<X size={16} />} disabled={pending} onClick={onClose} /></header>
      <textarea autoFocus required aria-label="Commit message" placeholder="Commit message" value={message}
        maxLength={4096} disabled={pending} onChange={(event) => setMessage(event.target.value)} />
      <label className="gitCommitOption"><input type="checkbox" checked={includeUnstaged} disabled={pending}
        onChange={(event) => setIncludeUnstaged(event.target.checked)} />Include unstaged changes <small>{files.length} files</small></label>
      <label className="gitCommitOption">Remote<select aria-label="Push remote" value={remote} disabled={pending}
        onChange={(event) => setRemote(event.target.value)}>
        <option value="">Select remote</option>{data.remotes.map((name) => <option key={name}>{name}</option>)}
      </select></label>
      {error && <p className="gitProblem" role="alert">{error}</p>}
      <footer>
        <button disabled={disabled || files.length === 0 || !message.trim()} onClick={() => request("commit")}><GitCommitHorizontal size={17} />Commit{pending && <LoaderCircle className="spin" size={15} />}</button>
        <button disabled={disabled || files.length === 0 || !remote || !message.trim()} onClick={() => request("commit_push")}><Upload size={17} />Commit and push</button>
        <button disabled={disabled || !remote} onClick={() => request("push")}><Upload size={17} />Push</button>
      </footer>
    </section>
  </div>;
}
