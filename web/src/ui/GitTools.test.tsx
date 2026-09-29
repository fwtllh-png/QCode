import {act, cleanup, fireEvent, render, screen, waitFor, within} from "@testing-library/react";
import {afterEach, expect, it, vi} from "vitest";
import type {GitActionResult, GitOverview, SessionSummary} from "../protocol";
import type {RuntimeClient} from "../runtime/client";
import {GitTools, gitTotals} from "./GitTools";
import {gitPatchRows, GitPatchView} from "./GitPatchView";

const overview: GitOverview = {
  revision: "index-1", head: "head-1", root: true,
  repository: true, branch: "main", branches: ["main", "feature"], remotes: ["origin"],
  files: [
    {path: "main.go", index: "M", worktree: "M", staged: {added: 2, removed: 1}, unstaged: {added: 3, removed: 0}},
    {path: "notes.txt", index: "?", worktree: "?", untracked: true, unstaged: {added: 4, removed: 0}}
  ]
};

function setup(mutationDisabled = "") {
  const client = {
    gitOverview: vi.fn(async () => overview),
    gitPatch: vi.fn(async (_workspace: string, path: string, staged: boolean) => ({path, staged, diff: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"})),
    executeGitAction: vi.fn(async (): Promise<GitActionResult> => ({action: "commit", completed: ["git_commit"], commit_hash: "commit-1"})),
    switchWorkspaceBranch: vi.fn(async () => {})
  };
  const onClose = vi.fn();
  const props = {
    client: client as unknown as RuntimeClient,
    workspace: {id: "workspace-a", label: "QCode", root: "/workspace-a", ready: true, removable: true, session_count: 1},
    session: {session_id: "session-a", isolation: "shared"} as SessionSummary,
    busy: false, mutationDisabled, modal: false, onClose
  };
  return {client, props, onClose};
}

afterEach(cleanup);

it("renders parsed Git hunks with actual line numbers and preserves binary metadata", () => {
  const rows = gitPatchRows("--- a/main.go\n+++ b/main.go\n@@ -3,2 +3,2 @@\n context\n-old\n+new\n");
  expect(rows[1]).toMatchObject({before: 3, after: 3, kind: "context"});
  expect(rows[2]).toMatchObject({before: 4, kind: "removed", text: "-old"});
  expect(rows[3]).toMatchObject({after: 4, kind: "added", text: "+new"});
  render(<GitPatchView diff={"diff --git a/image.png b/image.png\nBinary files a/image.png and b/image.png differ\n"} />);
  expect(screen.getByText(/Binary files/)).toBeTruthy();
});

it("shows only Git facts and separates staged and unstaged file previews", async () => {
  const {client, props} = setup();
  render(<GitTools {...props} />);
  await screen.findByRole("button", {name: /Changes/});
  expect(gitTotals(overview.files)).toEqual({added: 9, removed: 1});
  expect(screen.getByLabelText("9 additions, 1 deletions")).toBeTruthy();
  expect(screen.queryByText(/excluded from/)).toBeNull();
  expect(screen.queryByText("Progress")).toBeNull();
  expect(screen.queryByText("Agents")).toBeNull();
  fireEvent.click(screen.getByRole("button", {name: /Changes/}));
  expect(screen.getByLabelText("7 additions, 0 deletions")).toBeTruthy();
  const newFile = screen.getByRole("button", {name: /notes.txt/});
  expect(within(newFile).getByText("New")).toBeTruthy();
  expect(within(newFile).getByLabelText("4 additions, 0 deletions")).toBeTruthy();
  fireEvent.click(newFile);
  await waitFor(() => expect(client.gitPatch).toHaveBeenCalledWith("workspace-a", "notes.txt", false, expect.any(AbortSignal)));
  fireEvent.click(screen.getByRole("button", {name: /main.go/}));
  await waitFor(() => expect(client.gitPatch).toHaveBeenCalledWith("workspace-a", "main.go", false, expect.any(AbortSignal)));
  fireEvent.change(screen.getByRole("combobox", {name: "Git change scope"}), {target: {value: "staged"}});
  expect(screen.queryByRole("button", {name: /notes.txt/})).toBeNull();
  fireEvent.click(screen.getByRole("button", {name: /main.go/}));
  await waitFor(() => expect(client.gitPatch).toHaveBeenLastCalledWith("workspace-a", "main.go", true, expect.any(AbortSignal)));
});

it("distinguishes new binary files and unavailable counts from empty text files", async () => {
  const {client, props} = setup();
  client.gitOverview.mockResolvedValue({...overview, files: [
    ...overview.files,
    {path: "image.png", index: "?", worktree: "?", untracked: true, unstaged: {added: 0, removed: 0, binary: true}},
    {path: "empty.txt", index: "?", worktree: "?", untracked: true, unstaged: {added: 0, removed: 0}},
    {path: "large.txt", index: "?", worktree: "?", untracked: true}
  ]});
  render(<GitTools {...props} />);
  fireEvent.click(await screen.findByRole("button", {name: /Changes/}));
  expect(screen.getByText("New files with unavailable line counts: 1 (excluded from totals).")).toBeTruthy();
  expect(screen.getByLabelText("7 additions, 0 deletions (partial)")).toBeTruthy();
  expect(within(screen.getByRole("button", {name: /image.png/})).getByText("Binary")).toBeTruthy();
  expect(within(screen.getByRole("button", {name: /empty.txt/})).getByLabelText("0 additions, 0 deletions")).toBeTruthy();
  const unavailable = within(screen.getByRole("button", {name: /large.txt/}));
  expect(unavailable.getByText("Lines unavailable")).toBeTruthy();
  expect(unavailable.queryByText("+0")).toBeNull();
});

it("commits only staged paths by default and shows the actual revision", async () => {
  const {client, props} = setup();
  render(<div className="app"><GitTools {...props} /></div>);
  fireEvent.click(await screen.findByRole("button", {name: "Commit or push"}));
  const dialog = screen.getByRole("dialog", {name: "Commit or push"});
  expect((within(dialog).getByRole("checkbox") as HTMLInputElement).checked).toBe(false);
  expect((within(dialog).getByRole("button", {name: "Commit"}) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(within(dialog).getByRole("textbox", {name: "Commit message"}), {target: {value: "reviewed change"}});
  fireEvent.click(within(dialog).getByRole("button", {name: "Commit"}));
  await waitFor(() => expect(client.executeGitAction).toHaveBeenCalledWith("workspace-a", "session-a", {
    action: "commit", branch: "main", revision: "index-1", message: "reviewed change", include_unstaged: false, paths: ["main.go"], remote: undefined
  }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Committed commit-1"));
  expect(screen.queryByText(/sent to the conversation/)).toBeNull();
});

it("requires explicit inclusion of unstaged files and a named remote for push", async () => {
  const {client, props} = setup();
  client.gitOverview.mockResolvedValue({...overview, remotes: ["origin", "backup"]});
  render(<div className="app"><GitTools {...props} /></div>);
  fireEvent.click(await screen.findByRole("button", {name: "Commit or push"}));
  const push = screen.getByRole("button", {name: "Commit and push"});
  expect((push as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByRole("checkbox"));
  fireEvent.change(screen.getByRole("textbox", {name: "Commit message"}), {target: {value: "commit and push"}});
  fireEvent.change(screen.getByRole("combobox", {name: "Push remote"}), {target: {value: "backup"}});
  fireEvent.click(push);
  await waitFor(() => expect(client.executeGitAction).toHaveBeenCalledWith("workspace-a", "session-a", expect.objectContaining({
    action: "commit_push", remote: "backup", paths: ["main.go", "notes.txt"], include_unstaged: true
  })));
});

it("keeps the commit receipt visible when push fails", async () => {
  const {client, props} = setup();
  client.executeGitAction.mockResolvedValue({
    action: "commit_push", completed: ["git_commit"], commit_hash: "created-commit",
    problem: {version: 1, code: "conflict", message: "git_push failed", retryable: false}
  });
  render(<div className="app"><GitTools {...props} /></div>);
  fireEvent.click(await screen.findByRole("button", {name: "Commit or push"}));
  fireEvent.change(screen.getByRole("textbox", {name: "Commit message"}), {target: {value: "change"}});
  fireEvent.click(screen.getByRole("button", {name: "Commit and push"}));
  await waitFor(() => expect(screen.getByRole("status").textContent).toContain("created-commit"));
  expect(screen.getByRole("alert").textContent).toContain("git_push failed");
  expect(screen.queryByText(/Pushed to/)).toBeNull();
  expect(client.executeGitAction).toHaveBeenCalledTimes(1);
});

it("keeps query errors explicit without showing stale counts", async () => {
  const {client, props} = setup();
  render(<GitTools {...props} />);
  await screen.findByRole("button", {name: /Changes/});
  client.gitOverview.mockRejectedValue(new Error("Git unavailable"));
  fireEvent.click(screen.getByRole("button", {name: "Refresh Git"}));
  await screen.findByRole("alert");
  expect(screen.queryByRole("button", {name: /Changes/})).toBeNull();
});

it("aborts requests when dismissed and does not query on ordinary rerenders", async () => {
  const {client, props} = setup();
  const view = render(<GitTools {...props} />);
  await screen.findByRole("button", {name: /Changes/});
  view.rerender(<GitTools {...props} />);
  expect(client.gitOverview).toHaveBeenCalledTimes(1);
  const signal = (client.gitOverview.mock.calls as unknown as [string, AbortSignal][])[0][1];
  view.unmount();
  expect(signal.aborted).toBe(true);
});

it("blocks writes in read-only or isolated sessions while keeping changes available", async () => {
  const {props, client} = setup("Workspace actions require a shared session.");
  render(<GitTools {...props} />);
  expect((await screen.findByRole("button", {name: "Commit or push"}) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByRole("button", {name: /Changes/}));
  expect(screen.getByRole("combobox", {name: "Git change scope"})).toBeTruthy();
  expect(client.executeGitAction).not.toHaveBeenCalled();
});

it("searches local branches and does not switch during active work", async () => {
  const {props, client} = setup();
  const view = render(<GitTools {...props} />);
  fireEvent.click(await screen.findByRole("button", {name: "main"}));
  fireEvent.change(screen.getByRole("searchbox"), {target: {value: "feat"}});
  expect(screen.queryByRole("button", {name: "main"})).toBeNull();
  await act(async () => view.rerender(<GitTools {...props} busy />));
  expect((screen.getByRole("button", {name: "feature"}) as HTMLButtonElement).disabled).toBe(true);
  expect(client.switchWorkspaceBranch).not.toHaveBeenCalled();
});

it("keeps the compact mobile summary nonmodal until details are opened", async () => {
  const {props, onClose} = setup();
  render(<div className="app">
    <main><input aria-label="Draft" autoFocus /></main>
    <GitTools {...props} modal />
  </div>);
  const draft = screen.getByLabelText("Draft");
  const panel = await screen.findByRole("complementary", {name: "Git tools"});
  expect(document.activeElement).toBe(draft);
  fireEvent.keyDown(draft, {key: "Escape"});
  expect(onClose).not.toHaveBeenCalled();
  fireEvent.click(within(panel).getByRole("button", {name: /Changes/}));
  expect(screen.getByRole("dialog", {name: "Git tools"})).toBeTruthy();
  fireEvent.click(screen.getByRole("button", {name: "Back to Git tools"}));
  expect(screen.queryByRole("dialog", {name: "Git tools"})).toBeNull();
  expect(screen.getByRole("complementary", {name: "Git tools"})).toBeTruthy();
});
