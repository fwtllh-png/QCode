import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within
} from "@testing-library/react";
import {afterEach, describe, expect, it, vi} from "vitest";
import type {RuntimeEvent, SessionSummary} from "../protocol";
import {projectConversation} from "../projection/conversation";
import type {RuntimeClient, RuntimeSnapshot} from "../runtime/client";
import {App, projectTranscript, selectionRange} from "./App";
import {notificationPreferenceKey} from "./browserNotifications";

const clipboardWrite = vi.fn(async () => {});

Object.defineProperty(HTMLElement.prototype, "scrollTo", {
  configurable: true,
  value: vi.fn()
});
Object.defineProperty(navigator, "clipboard", {
  configurable: true,
  value: {writeText: clipboardWrite}
});
Object.defineProperty(URL, "createObjectURL", {
  configurable: true,
  value: vi.fn(() => "blob:workspace-resource")
});
Object.defineProperty(URL, "revokeObjectURL", {
  configurable: true,
  value: vi.fn()
});

afterEach(() => {
  cleanup();
  clipboardWrite.mockClear();
  document.title = "QCode";
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("selectionRange", () => {
  it("converts browser offsets into zero-based protocol positions", () => {
    expect(selectionRange("alpha\nbeta\ngamma", 2, 9)).toEqual({
      start: {line: 0, character: 2},
      end: {line: 1, character: 3}
    });
    expect(selectionRange("alpha", 3, 3)).toBeUndefined();
  });
});

describe("projectTranscript", () => {
  it("withdraws only the latest turn after confirmation", async () => {
    const value = snapshot([
      {...event(1, "turn.started", {prompt: "previous"}), turn_id: "previous"},
      {...event(2, "turn.completed", {text: "done"}), turn_id: "previous"},
      event(3, "turn.started", {prompt: "mistaken request"}),
      event(4, "turn.canceled", {reason: "user_interrupted"})
    ]);
    value.sessions = [{...value.sessions[0], latest_turn_id: "turn"}];
    const client = mockClient(value);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<App client={client} />);
    expect(screen.getAllByRole("button", {name: "Withdraw turn"})).toHaveLength(1);
    const request = screen.getByText("mistaken request").closest(".userMessageGroup");
    expect(request).not.toBeNull();
    expect(within(request as HTMLElement).getByRole("button", {name: "Withdraw turn"})).toBeTruthy();
    expect(screen.getByText("previous").closest(".userMessageGroup")?.querySelector("button")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Withdraw turn"}));
    expect(client.withdrawTurn).not.toHaveBeenCalled();
    expect(screen.getByRole("alertdialog", {name: "Withdraw this turn?"})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Cancel"}));
    expect(screen.queryByRole("alertdialog", {name: "Withdraw this turn?"})).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Withdraw turn"}));
    fireEvent.click(screen.getByRole("button", {name: "Withdraw"}));
    await waitFor(() => expect(client.withdrawTurn).toHaveBeenCalledWith("turn"));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("collapses withdrawn history with an expandable audit and no recovery actions", () => {
    const value = snapshot([
      event(1, "turn.started", {prompt: "mistaken request"}),
      event(2, "turn.canceled", {reason: "user_interrupted"}),
      event(3, "turn.withdrawn", {})
    ]);
    value.sessions = [{...value.sessions[0], latest_turn_id: "turn", latest_turn_withdrawn: true}];
    render(<App client={mockClient(value)} />);
    expect(screen.queryByText("mistaken request")).toBeNull();
    const audit = screen.getByRole("button", {name: "Turn withdrawn Excluded from context"});
    expect(audit.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(audit);
    expect(screen.getByText("mistaken request")).toBeTruthy();
    expect(screen.getByText("mistaken request").closest(".userMessageGroup")?.textContent)
      .toContain("Withdrawn");
    expect(screen.queryByRole("button", {name: "Withdraw turn"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Retry"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Continue"})).toBeNull();
  });

  it("keeps the confirmation open on failure and allows retry without hiding history", async () => {
    const value = snapshot([event(1, "turn.started", {prompt: "mistaken request"})]);
    value.sessions = [{...value.sessions[0], latest_turn_id: "turn"}];
    const client = mockClient(value);
    vi.mocked(client.withdrawTurn).mockRejectedValueOnce(new Error("No saved pre-Turn context"));
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Withdraw turn"}));
    fireEvent.click(screen.getByRole("button", {name: "Withdraw"}));
    const failure = await screen.findByRole("alert");
    expect(failure.textContent).toContain("No saved pre-Turn context");
    expect(failure.closest('[role="alertdialog"]')).toBeTruthy();
    expect(screen.getByText("mistaken request")).toBeTruthy();
    expect(screen.queryByText("Turn withdrawn")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Withdraw"}));
    await waitFor(() => expect(client.withdrawTurn).toHaveBeenCalledTimes(2));
  });

  it("keeps withdrawal pending until the request settles and prevents duplicate confirmation", async () => {
    const value = snapshot([event(1, "turn.started", {prompt: "mistaken request"})]);
    value.sessions = [{...value.sessions[0], latest_turn_id: "turn"}];
    const client = mockClient(value);
    let finish!: () => void;
    vi.mocked(client.withdrawTurn).mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Withdraw turn"}));
    fireEvent.click(screen.getByRole("button", {name: "Withdraw"}));
    const pending = screen.getByRole("button", {name: "Withdrawing..."}) as HTMLButtonElement;
    expect(pending.disabled).toBe(true);
    fireEvent.click(pending);
    expect(client.withdrawTurn).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Turn withdrawn")).toBeNull();
    await act(async () => finish());
  });

  it("reconciles streamed output with the authoritative terminal text", () => {
    const entries = projectTranscript([
      event(1, "output.delta", {text: "draft"}),
      event(2, "turn.completed", {text: "final", outcome: "answered"})
    ]);

    expect(entries).toMatchObject([
      {kind: "assistant", text: "final"}
    ]);
  });

  it("does not duplicate a complete tool result after streamed chunks", () => {
    const entries = projectTranscript([
      event(1, "tool.start", {call_id: "call", tool: "read", arguments: {path: "a.go"}}),
      event(2, "tool.output", {call_id: "call", chunk: "content"}),
      event(3, "tool.result", {call_id: "call", output: "content", is_error: false})
    ]);

    expect(entries).toMatchObject([
      {
        kind: "tool",
        title: "Read",
        output: "content",
        state: "completed",
        callID: "call",
        contextText: "content"
      }
    ]);
  });

  it("claims an automatic title when rename confirms the same text", async () => {
    const value = snapshot();
    const session = {...value.sessions[0]!, title_source: "auto" as const, title_revision: 3};
    const client = mockClient({...value, sessions: [session]});
    const prompt = vi.spyOn(window, "prompt").mockReturnValue(session.title);
    try {
      render(<App client={client} />);
      fireEvent.click(screen.getByRole("button", {name: `Session actions for ${session.title}`}));
      fireEvent.click(screen.getByRole("menuitem", {name: "Rename"}));
      await waitFor(() => expect(client.updateSession).toHaveBeenCalledWith(
        session.session_id, session.revision, {title: session.title}
      ));
    } finally {
      prompt.mockRestore();
    }
  });

  it("projects verification, receipt, and rejection evidence", () => {
    const entries = projectTranscript([
      event(1, "turn.verification", {verdict: "passed"}),
      event(2, "turn.receipt", {outcome: "changed"}),
      event(3, "operation.rejected", {message: "stale request"})
    ]);

    expect(entries).toMatchObject([
      {
        kind: "status",
        title: "Checks passed",
        text: "Changed files are covered by recorded checks.",
        failed: false
      },
      {kind: "receipt", data: {outcome: "changed"}},
      {kind: "status", title: "Rejected", text: "stale request", failed: true}
    ]);
  });

  it("retains the source turn for failed and canceled recovery actions", () => {
    const entries = projectTranscript([
      event(1, "turn.failed", {message: "provider unavailable"}),
      event(2, "turn.canceled", {reason: "interrupted"})
    ]);

    expect(entries).toMatchObject([
      {kind: "status", title: "Failed", failed: true, turnID: "turn"},
      {kind: "status", title: "Canceled", failed: true, turnID: "turn"}
    ]);
  });

  it("reports failed recovery operations", async () => {
    const client = mockClient(snapshot([
      event(1, "turn.failed", {message: "provider unavailable"})
    ]));
    vi.mocked(client.recoverTurn).mockRejectedValue(
      new Error("recovery was rejected")
    );
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Retry"}));

    expect(await screen.findByText("recovery was rejected")).toBeTruthy();
    expect(client.recoverTurn).toHaveBeenCalledWith("turn", "retry");
  });

  it("renders Runtime recovery capabilities at the failed turn", async () => {
    const value = snapshot([
      event(1, "turn.failed", {
        code: "internal",
        message: "verification failed",
        fault: {
          disposition: "resume_turn",
          side_effects: "committed",
          recovery_action: "inspect the workspace before continuing"
        }
      })
    ]);
    value.checkpoints = [{
      version: 2,
      id: "checkpoint-failed",
      session_id: "session",
      thread_id: "thread",
      turn_id: "turn",
      cursor: 1,
      status: "interrupted",
      summary: "Before failure",
      profile_revision: 1,
      changed_files: 1,
      external_side_effects: true,
      can_restore: true,
      can_fork: true,
      created_at: "2026-01-01T00:00:00Z"
    }];
    const client = mockClient(value);
    render(<App client={client} />);

    expect(screen.queryByRole("button", {name: "Retry"})).toBeNull();
    expect(screen.getByText("Workspace changes were kept.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Restore"}));
    await waitFor(() => {
      expect(client.restoreCheckpoint).toHaveBeenCalledWith("checkpoint-failed");
    });
    fireEvent.click(screen.getByRole("button", {name: "Fork"}));
    await waitFor(() => {
      expect(client.forkCheckpoint).toHaveBeenCalledWith("checkpoint-failed");
    });

    vi.mocked(client.recoverTurn).mockImplementation(
      () => new Promise(() => undefined)
    );
    fireEvent.click(screen.getByRole("button", {name: "Continue"}));
    await waitFor(() => {
      expect(client.recoverTurn).toHaveBeenCalledWith(
        "turn",
        "continue"
      );
    });
    expect(
      screen.getByRole("button", {name: "Continue"}).getAttribute("disabled")
    ).not.toBeNull();
  });

  it("routes blocked-session composer input into Turn recovery", async () => {
    const value = snapshot([
      event(1, "turn.failed", {
        code: "unavailable",
        message: "workspace journal has a retained draft",
        fault: {
          disposition: "resume_turn",
          side_effects: "draft"
        }
      })
    ]);
    value.sessions = value.sessions.map((session, index) => index === 0
      ? {...session, status: "blocked", latest_turn_id: "turn"}
      : session);
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.change(screen.getByPlaceholderText("Ask QCode"), {
      target: {value: "Run the remaining checks"}
    });
    const buttons = screen.getAllByRole("button", {name: "Continue"});
    fireEvent.click(buttons.at(-1)!);

    await waitFor(() => {
      expect(client.recoverTurn).toHaveBeenCalledWith(
        "turn",
        "continue",
        "Run the remaining checks"
      );
    });
    expect(client.submitPrompt).not.toHaveBeenCalled();
  });

  it("routes paused-session composer input into Turn recovery", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Implement the change"}),
      event(2, "turn.canceled", {reason: "user_interrupted"})
    ]);
    value.sessions = value.sessions.map((session, index) => index === 0
      ? {...session, status: "interrupted", latest_turn_id: "turn"}
      : session);
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.change(screen.getByPlaceholderText("Ask QCode"), {
      target: {value: "Continue with the remaining implementation"}
    });
    const buttons = screen.getAllByRole("button", {name: "Continue"});
    fireEvent.click(buttons.at(-1)!);

    await waitFor(() => {
      expect(client.recoverTurn).toHaveBeenCalledWith(
        "turn",
        "continue",
        "Continue with the remaining implementation"
      );
    });
    expect(client.submitPrompt).not.toHaveBeenCalled();
  });

  it("offers recovery actions only for the latest interrupted turn", async () => {
    const oldStarted = {
      ...event(1, "turn.started", {display_prompt: "Old request"}),
      turn_id: "turn-old"
    };
    const oldCanceled = {
      ...event(2, "turn.canceled", {reason: "user_interrupted"}),
      turn_id: "turn-old"
    };
    const latestStarted = {
      ...event(3, "turn.started", {display_prompt: "Latest request"}),
      turn_id: "turn-latest"
    };
    const latestCanceled = {
      ...event(4, "turn.canceled", {reason: "user_interrupted"}),
      turn_id: "turn-latest"
    };
    const value = snapshot([
      oldStarted,
      oldCanceled,
      latestStarted,
      latestCanceled
    ]);
    value.sessions = value.sessions.map((session, index) => index === 0
      ? {
          ...session,
          status: "interrupted",
          latest_turn_id: "turn-latest"
        }
      : session);
    const client = mockClient(value);
    render(<App client={client} />);

    const retry = screen.getByRole("button", {name: "Retry"});
    expect(screen.getAllByRole("button", {name: "Retry"})).toHaveLength(1);
    fireEvent.click(retry);
    await waitFor(() => {
      expect(client.recoverTurn).toHaveBeenCalledWith("turn-latest", "retry");
    });
  });

  it("filters the sidebar without hiding the active conversation", () => {
    const value = snapshot([
      event(1, "turn.completed", {text: "Active conversation content"})
    ]);
    value.sessionSearchQuery = "no match";
    const client = mockClient(value);
    render(<App client={client} />);
    expect((screen.getByRole("textbox", {name: "Search sessions"}) as HTMLInputElement).value)
      .toBe("no match");
    expect(document.querySelectorAll(".sessionGroup .sessionRow")).toHaveLength(0);
    expect(screen.getByText("Active conversation content")).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("textbox", {name: "Search sessions"}), {key: "Escape"});
    expect(client.refreshSessions).toHaveBeenCalledWith("");
    expect(client.selectSession).not.toHaveBeenCalled();
  });

  it("renders lifecycle, workspace, profile, and governed tool controls", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);
    expect(screen.getByRole("button", {name: "New session in workspace"})).toBeTruthy();
    expect(screen.getByRole("button", {name: "Add workspace"}).textContent)
      .toContain("Add workspace");
    expect(screen.getByRole("button", {name: /^workspace/})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Search sessions"}));
    expect(screen.getByRole("textbox", {name: "Search sessions"})).toBeTruthy();
    await openContextDetails();

    expect(screen.getByRole("dialog", {name: "Add context"})).toBeTruthy();
    expect(screen.queryByLabelText("Session details")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Close context browser"}));
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    expect(await screen.findByRole("dialog", {name: "Settings"})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Models"}));
    expect((screen.getByLabelText("Settings model") as HTMLSelectElement).value)
      .toBe("fixture");
    fireEvent.change(screen.getByLabelText("Settings model"), {
      target: {value: "reasoner"}
    });
    expect((screen.getByLabelText("Settings model") as HTMLSelectElement).value)
      .toBe("reasoner");
    fireEvent.click(screen.getByRole("button", {name: "Add model"}));
    const modelDialog = screen.getByRole("dialog", {name: "Add model"});
    expect((within(modelDialog).getByLabelText("New model ID") as HTMLInputElement).value)
      .toBe("");
    expect(document.activeElement).toBe(
      within(modelDialog).getByLabelText("New model ID")
    );
    expect(within(modelDialog).getByRole("alert").textContent).toContain(
      "Model ID is required"
    );
    fireEvent.change(within(modelDialog).getByLabelText("New model ID"), {
      target: {value: "invalid model"}
    });
    expect(within(modelDialog).getByRole("alert").textContent).toContain(
      "Model ID cannot contain whitespace"
    );
    fireEvent.change(within(modelDialog).getByLabelText("New model ID"), {
      target: {value: "model-released-today"}
    });
    fireEvent.click(within(modelDialog).getByRole(
      "button",
      {name: "Detect model"}
    ));
    expect(
      (await within(modelDialog).findByLabelText("Detected model limits"))
        .textContent
    ).toContain("Context 200,000");
    expect(within(modelDialog).queryByLabelText("Context tokens")).toBeNull();
    fireEvent.click(within(modelDialog).getByRole(
      "button",
      {name: "Add model"}
    ));
    await waitFor(() => {
      expect(client.addModel).toHaveBeenCalledWith(expect.objectContaining({
        model: "model-released-today"
      }));
    });
    expect(client.updateProfile).not.toHaveBeenCalled();
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Apply changes"}));
    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({
        model: "model-released-today",
        reasoning_effort: "medium"
      });
    });
    expect(screen.getByText(
      "Applied: Model fixture → model-released-today; " +
      "Reasoning default → medium · Prompt cache reset"
    )).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Connection"}));
    expect(screen.getByRole("button", {name: "Test connection"})).toBeTruthy();
    expect(await screen.findByText("https://models.example.com/v1")).toBeTruthy();
    expect(screen.getByText("openai_chat")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Tools"}));
    expect(screen.getByText("read_file")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Agent preset"}));
    expect(screen.getByLabelText("Agent mode")).toBeTruthy();
  });

  it("adds a Workspace through the managed selector", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Add workspace"}));
    const dialog = screen.getByRole("dialog", {name: "Workspaces"});
    expect(dialog.querySelector("input")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Choose folder"}));

    await waitFor(() => {
      expect(client.pickWorkspaceDirectory).toHaveBeenCalledTimes(1);
      expect(client.addWorkspace).toHaveBeenCalledWith("/workspace/secondary");
    });
  });

  it("keeps the Workspace selector open when native selection is cancelled", async () => {
    const client = mockClient(snapshot());
    vi.mocked(client.pickWorkspaceDirectory).mockResolvedValueOnce({
      cancelled: true
    });
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Add workspace"}));
    fireEvent.click(screen.getByRole("button", {name: "Choose folder"}));

    await waitFor(() => {
      expect(client.pickWorkspaceDirectory).toHaveBeenCalledTimes(1);
    });
    expect(client.addWorkspace).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", {name: "Workspaces"})).toBeTruthy();
  });

  it("removes a Workspace from the sidebar after explicit confirmation", async () => {
    const value = snapshot();
    value.workspaces = [
      ...value.workspaces,
      {
        id: "workspace-secondary",
        root: "/workspace/secondary",
        label: "secondary",
        ready: true,
        removable: true,
        session_count: 0
      }
    ];
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole(
      "button",
      {name: "Remove secondary"}
    ));
    expect(client.removeWorkspace).not.toHaveBeenCalled();
    expect(screen.getByRole("alertdialog", {name: "Remove workspace?"}))
      .toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Cancel"}));
    expect(client.removeWorkspace).not.toHaveBeenCalled();
    expect(screen.queryByRole("alertdialog")).toBeNull();

    fireEvent.click(screen.getByRole(
      "button",
      {name: "Remove secondary"}
    ));
    fireEvent.click(within(screen.getByRole("alertdialog")).getByRole(
      "button",
      {name: "Remove workspace"}
    ));

    await waitFor(() => {
      expect(client.removeWorkspace).toHaveBeenCalledWith("workspace-secondary");
    });
  });

  it("allows removing the only Workspace after explicit confirmation", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Remove workspace"}));
    expect(client.removeWorkspace).not.toHaveBeenCalled();
    fireEvent.click(within(screen.getByRole("alertdialog")).getByRole(
      "button",
      {name: "Remove workspace"}
    ));

    await waitFor(() => {
      expect(client.removeWorkspace).toHaveBeenCalledWith("workspace-id");
    });
  });

  it("delegates removal of the selected Workspace after confirmation", async () => {
    const value = snapshot();
    value.workspaces = [
      ...value.workspaces,
      {
        id: "workspace-secondary",
        root: "/workspace/secondary",
        label: "secondary",
        ready: true,
        removable: true,
        session_count: 0
      }
    ];
    value.selectedWorkspaceID = "workspace-secondary";
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole(
      "button",
      {name: "Remove secondary"}
    ));
    fireEvent.click(within(screen.getByRole("alertdialog")).getByRole(
      "button",
      {name: "Remove workspace"}
    ));

    await waitFor(() => {
      expect(client.removeWorkspace).toHaveBeenCalledWith("workspace-secondary");
    });
  });

  it("shows Git tools by default and switches branches without a sidebar selector", async () => {
    const value = snapshot();
    value.workspaces[0]!.git = {
      repository: true,
      branch: "main",
      branches: ["feature", "main"]
    };
    const client = mockClient(value);
    render(<App client={client} />);

    const panel = await screen.findByRole("complementary", {name: "Git tools"});
    expect(screen.queryByLabelText("Branch for workspace")).toBeNull();
    fireEvent.click(await within(panel).findByRole("button", {name: "main"}));
    fireEvent.click(within(panel).getByRole("button", {name: "feature"}));
    await waitFor(() => {
      expect(client.switchWorkspaceBranch)
        .toHaveBeenCalledWith("workspace-id", "feature");
    });
  });

  it("keeps Git dismissal within a Workspace and reopens for another Workspace", async () => {
    const value = snapshot();
    const client = mockClient(value);
    const view = render(<App client={client} />);
    const panel = await screen.findByRole("complementary", {name: "Git tools"});
    fireEvent.click(within(panel).getByRole("button", {name: "Close Git tools"}));
    await waitFor(() => expect(screen.queryByRole("complementary", {name: "Git tools"})).toBeNull());
    const nextSession = {...value, selectedSessionID: "session-other"};
    view.rerender(<App client={mockClient(nextSession)} />);
    expect(screen.queryByRole("complementary", {name: "Git tools"})).toBeNull();
    const workspace = {
      ...value.workspaces[0]!, id: "workspace-other", root: "/other", label: "other"
    };
    const nextClient = mockClient({
      ...nextSession, workspaces: [...value.workspaces, workspace], selectedWorkspaceID: workspace.id
    });
    view.rerender(<App client={nextClient} />);
    const nextPanel = await screen.findByRole("complementary", {name: "Git tools"});
    expect(nextPanel.textContent).toContain("other");
    await waitFor(() => expect(nextClient.gitOverview).toHaveBeenCalledWith("workspace-other", expect.any(AbortSignal)));
  });

  it("keeps branch conflicts visible in Git tools", async () => {
    const value = snapshot();
    value.workspaces[0]!.git = {
      repository: true,
      branch: "main",
      branches: ["feature", "main"],
      dirty: true
    };
    const client = mockClient(value);
    vi.mocked(client.switchWorkspaceBranch).mockRejectedValue(new Error(
      "git switch: error: Your local changes to README.md would be overwritten by checkout"
    ));
    render(<App client={client} />);

    const panel = await screen.findByRole("complementary", {name: "Git tools"});
    fireEvent.click(await within(panel).findByRole("button", {name: "main"}));
    fireEvent.click(within(panel).getByRole("button", {name: "feature"}));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("README.md would be overwritten");
    expect(within(panel).getByRole("searchbox", {name: "Search Git branches"})).toBeTruthy();
  });

  it("switches Session models from the composer and opens new model settings", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);

    fireEvent.change(screen.getByLabelText("Model"), {
      target: {value: "reasoner"}
    });
    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({
        model: "reasoner",
        reasoning_effort: ""
      });
    });

    fireEvent.change(screen.getByLabelText("Model"), {
      target: {value: "__configure__"}
    });
    expect(await screen.findByRole("dialog", {name: "Settings"})).toBeTruthy();
    expect(screen.getByRole("heading", {name: "Models"})).toBeTruthy();
  });

  it("offers three modes without exposing the derived planning policy", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);

    expect(Array.from(
      (screen.getByLabelText("Mode") as HTMLSelectElement).options
    ).map((option) => option.value)).toEqual(["plan", "act", "operate"]);
    fireEvent.change(screen.getByLabelText("Mode"), {
      target: {value: "operate"}
    });
    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({
        mode: "operate"
      });
    });
  });

  it("blocks duplicate composer profile updates while one is pending", async () => {
    const value = snapshot();
    const client = mockClient(value);
    let finish!: () => void;
    vi.mocked(client.updateProfile).mockImplementation(() => new Promise((resolve) => {
      finish = () => resolve({
        profile: {...value.profile!.profile, model: "reasoner", revision: 2},
        prompt_cache_reset: true,
        reset_reason: "model"
      });
    }));
    render(<App client={client} />);

    fireEvent.change(screen.getByLabelText("Model"), {
      target: {value: "reasoner"}
    });
    expect(await screen.findByText("Updating model")).toBeTruthy();
    expect((screen.getByLabelText("Model") as HTMLSelectElement).disabled).toBe(true);
    finish();
    await waitFor(() => {
      expect(screen.queryByText("Updating model")).toBeNull();
    });
  });

  it("opens the model editor for a fixed custom connection", async () => {
    const value = snapshot();
    value.profile!.capabilities.mutable_fields =
      value.profile!.capabilities.mutable_fields.filter(
        (field) => field !== "model"
      );
    value.models[0]!.capabilities.selection_mode = "fixed";
    const client = mockClient(value);
    render(<App client={client} />);

    const selector = screen.getByLabelText("Model") as HTMLSelectElement;
    expect(selector.disabled).toBe(false);
    fireEvent.change(selector, {target: {value: "__configure__"}});

    expect(await screen.findByRole("dialog", {name: "Settings"})).toBeTruthy();
    expect(screen.getByRole("dialog", {name: "Add model"})).toBeTruthy();
    expect(client.updateProfile).not.toHaveBeenCalled();
  });

  it("changes the advertised reasoning effort from the composer menu", async () => {
    const value = snapshot();
    value.models[0]!.capabilities = {
      ...value.models[0]!.capabilities,
      reasoning: true,
      reasoning_efforts: ["off", "low", "high", "max"],
      default_reasoning_effort: "high"
    };
    const client = mockClient(value);
    render(<App client={client} />);

    const trigger = screen.getByRole("button", {name: "Reasoning"});
    expect(trigger.textContent).toContain("High");
    fireEvent.click(trigger);

    expect(screen.getAllByRole("menuitemradio").map((item) => item.textContent))
      .toEqual(["Off", "Low", "High", "Max"]);
    expect(screen.queryByRole("menuitemradio", {name: "Medium"})).toBeNull();
    expect(screen.getByRole("menuitemradio", {name: "High"}).getAttribute("aria-checked"))
      .toBe("true");

    fireEvent.click(screen.getByRole("menuitemradio", {name: "Low"}));
    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({reasoning_effort: "low"});
    });
  });

  it("keeps every reasoning level advertised by other models", () => {
    const value = snapshot();
    value.models[0]!.capabilities = {
      ...value.models[0]!.capabilities,
      reasoning: true,
      reasoning_efforts: ["minimal", "low", "medium", "high", "xhigh", "max"]
    };
    render(<App client={mockClient(value)} />);

    fireEvent.click(screen.getByRole("button", {name: "Reasoning"}));
    expect(screen.getAllByRole("menuitemradio").map((item) => item.textContent))
      .toEqual(["Default", "Minimal", "Low", "Medium", "High", "XHigh", "Max"]);
  });

  it("projects background activity and opens privacy-safe browser notifications", async () => {
    class TestNotification {
      static permission: NotificationPermission = "default";
      static instances: TestNotification[] = [];
      static requestPermission = vi.fn(async () => {
        TestNotification.permission = "granted";
        return "granted" as NotificationPermission;
      });
      onclick: ((event: Event) => unknown) | null = null;
      onclose: ((event: Event) => unknown) | null = null;

      constructor(
        readonly title: string,
        readonly options?: NotificationOptions
      ) {
        TestNotification.instances.push(this);
      }

      close(): void {
        this.onclose?.(new Event("close"));
      }
    }
    vi.stubGlobal("Notification", TestNotification);
    const preferences = new Map<string, string>();
    vi.stubGlobal("localStorage", {
      getItem: (key: string) => preferences.get(key) ?? null,
      setItem: (key: string, value: string) => preferences.set(key, value),
      removeItem: (key: string) => preferences.delete(key)
    });
    const focus = vi.spyOn(window, "focus").mockImplementation(() => {});
    const foreground = snapshot();
    const background: SessionSummary = {
      ...foreground.sessions[0],
      session_id: "session-background",
      thread_id: "thread-background",
      title: "Private prompt title",
      status: "running",
      latest_turn_id: "turn-background"
    };
    let current = {
      ...foreground,
      sessions: [...foreground.sessions, background]
    };
    const listeners = new Set<() => void>();
    const client = mockClient(current);
    Object.assign(client, {
      subscribe: (listener: () => void) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
      getSnapshot: () => current
    });
    render(<App client={client} />);

    expect(document.title).toBe("(1) Working · QCode");
    const backgroundRow = screen.getByText("Private prompt title")
      .closest(".sessionRow");
    expect(backgroundRow?.querySelector('[title="Running"]')).toBeTruthy();

    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    const notificationSwitch = await screen.findByRole("switch", {
      name: "Desktop notifications"
    });
    expect(notificationSwitch).toHaveProperty("checked", false);
    expect(screen.getByText(/Prompts and tool output are excluded/)).toBeTruthy();
    fireEvent.click(notificationSwitch);
    await waitFor(() => {
      expect(notificationSwitch).toHaveProperty("checked", true);
    });
    expect(TestNotification.requestPermission).toHaveBeenCalledOnce();
    expect(preferences.get(notificationPreferenceKey)).toBe("true");
    fireEvent.click(screen.getByRole("button", {name: "Close settings"}));

    act(() => {
      current = {
        ...current,
        sessions: current.sessions.map((session) =>
          session.session_id === background.session_id
            ? {
                ...session,
                status: "awaiting_approval",
                pending_approvals: 1,
                latest_sequence: 2
              }
            : session
        )
      };
      for (const listener of listeners) listener();
    });

    await waitFor(() => expect(TestNotification.instances).toHaveLength(1));
    expect(document.title).toBe("(1) Action required · QCode");
    expect(TestNotification.instances[0]).toMatchObject({
      title: "QCode needs approval",
      options: {
        body: "A background Session is waiting for approval."
      }
    });
    expect(JSON.stringify(TestNotification.instances[0].options))
      .not.toContain(background.title);
    expect(backgroundRow?.querySelector('[title="Approval required"]')).toBeTruthy();

    TestNotification.instances[0].onclick?.(new Event("click"));
    expect(focus).toHaveBeenCalledOnce();
    expect(client.selectSession).toHaveBeenCalledWith(background.session_id);
  });

  it("shows every Runtime background state in the Session rail", () => {
    const value = snapshot();
    value.sessions = [
      {...value.sessions[0], title: "Running task", status: "running"},
      {
        ...value.sessions[0],
        session_id: "approval",
        thread_id: "thread-approval",
        title: "Approval task",
        status: "awaiting_approval",
        pending_approvals: 1
      },
      {
        ...value.sessions[0],
        session_id: "failed",
        thread_id: "thread-failed",
        title: "Failed task",
        status: "failed"
      },
      {
        ...value.sessions[0],
        session_id: "paused",
        thread_id: "thread-paused",
        title: "Paused task",
        status: "interrupted"
      },
      {
        ...value.sessions[0],
        session_id: "completed",
        thread_id: "thread-completed",
        title: "Completed task",
        status: "completed"
      }
    ];
    render(<App client={mockClient(value)} />);

    for (const [title, status] of [
      ["Running task", "Running"],
      ["Approval task", "Approval required"],
      ["Failed task", "Failed"],
      ["Paused task", "Paused"],
      ["Completed task", "Completed"]
    ]) {
      const row = Array.from(document.querySelectorAll(".sessionRow")).find(
        (item) => item.querySelector(".sessionTitle")?.textContent === title
      );
      expect(row?.querySelector(`[title="${status}"]`)).toBeTruthy();
    }
  });

  it("renders run statistics as one readable line", () => {
    const value = snapshot([
      event(1, "turn.receipt", {
        outcome: "answered",
        latency: {
          total_ms: 36_423,
          provider_ms: 35_000,
          tool_ms: 388,
          first_token_ms: 1_340
        },
        input_tokens: 115_465,
        output_tokens: 3_742,
        reasoning_tokens: 694,
        cached_tokens: 52_224,
        cost_known: false
      })
    ]);
    value.usage = {
      turns: 1,
      calls: 9,
      total_tokens: 119_901,
      cost_microunits: 0,
      cost_known: false
    };
    const {container} = render(<App client={mockClient(value)} />);

    const stats = screen.getByLabelText(/^Run statistics:/);
    expect(stats.textContent).toBe(
      "1 turn · 0 tools | 36.4 s total · 35.0 s model · 388 ms tools | " +
      "1.34 s TTFT | 119.2K tokens · 45% cache"
    );
    expect(container.querySelectorAll(".composerMeta > span")).toHaveLength(1);
    expect(stats.getAttribute("title")).toContain("115,465 in");
    expect(stats.getAttribute("title")).toContain("63,241 uncached");
  });

  it("shows a zero cache hit rate instead of hiding a cold sample", () => {
    render(<App client={mockClient(snapshot([
      event(1, "turn.receipt", {
        input_tokens: 100,
        output_tokens: 5,
        cached_tokens: 0
      })
    ]))} />);

    expect(screen.getByLabelText(/^Run statistics:/).textContent)
      .toContain("0% cache");
  });

  it("creates a new session without replaying first-run setup", async () => {
    const value = snapshot();
    value.sessions = [];
    value.selectedSessionID = "";
    value.profile = undefined;
    const client = mockClient(value);
    render(<App client={client} />);

    expect(screen.queryByPlaceholderText("Create a chat to begin")).toBeNull();
    expect(screen.queryByPlaceholderText("Ask QCode")).toBeNull();
    expect(screen.getByRole("heading", {name: "Start a new session"})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Create session"}));

    await waitFor(() => {
      expect(client.createSession).toHaveBeenCalledWith("shared", undefined);
    });
  });

  it("creates a session from its owning Workspace instead of the current Workspace", async () => {
    const value = snapshot();
    value.workspaces = [
      ...value.workspaces,
      {
        id: "workspace-secondary",
        root: "/workspace/secondary",
        label: "secondary",
        ready: true,
        removable: true,
        session_count: 0
      }
    ];
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "New session in secondary"}));

    await waitFor(() => {
      expect(client.selectWorkspace).toHaveBeenCalledWith("workspace-secondary");
      expect(client.createSession).toHaveBeenCalledWith("shared", undefined);
    });
    expect(vi.mocked(client.selectWorkspace).mock.invocationCallOrder[0])
      .toBeLessThan(vi.mocked(client.createSession).mock.invocationCallOrder[0]);
  });

  it("guides Workspace selection before creating a session", () => {
    const value = snapshot();
    value.sessions = [];
    value.selectedSessionID = "";
    value.selectedWorkspaceID = "";
    value.workspaceRoot = "";
    value.profile = undefined;
    const client = mockClient(value);
    render(<App client={client} />);

    expect(screen.getByRole("heading", {name: "Choose a workspace"})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Choose workspace"}));
    expect(screen.getByRole("dialog", {name: "Workspaces"})).toBeTruthy();
    expect(client.createSession).not.toHaveBeenCalled();
  });

  it("requires provider, model, and API key during first-run setup", async () => {
    const value = snapshot();
    value.phase = "setup";
    value.sessions = [];
    value.selectedSessionID = "";
    value.profile = undefined;
    value.setupCatalog = {
      version: 2,
      providers: [{
        id: "deepseek",
        display_name: "DeepSeek",
        protocol: "openai_chat",
        requires_api_key: true,
        models: ["deepseek-reasoner"]
      }, {
        id: "openai-compatible",
        display_name: "OpenAI-compatible",
        protocol: "openai_chat",
        requires_api_key: false,
        custom: true
      }]
    };
    const client = mockClient(value);
    render(<App client={client} />);

    expect(screen.getByRole("button", {name: "Start QCode"}))
      .toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Provider"), {
      target: {value: "deepseek"}
    });
    fireEvent.change(screen.getByLabelText("Model ID"), {
      target: {value: "deepseek-reasoner"}
    });
    const key = screen.getByLabelText("API key");
    fireEvent.change(key, {
      target: {value: "DEEPSEEK_API_KEY=secret"}
    });
    expect(screen.getByText(
      "Enter the API key value, not an environment assignment."
    )).toBeTruthy();
    expect(screen.getByRole("button", {name: "Start QCode"}))
      .toHaveProperty("disabled", true);

    fireEvent.change(key, {target: {value: "sk-live"}});
    fireEvent.click(screen.getByRole("button", {name: "Start QCode"}));

    await waitFor(() => {
      expect(client.completeSetup).toHaveBeenCalledWith({
        provider: "deepseek",
        model: "deepseek-reasoner",
        api_key: "sk-live"
      });
    });
    expect(screen.queryByDisplayValue("sk-live")).toBeNull();
  });

  it("detects and submits custom provider metadata", async () => {
    const value = snapshot();
    value.phase = "setup";
    value.sessions = [];
    value.selectedSessionID = "";
    value.profile = undefined;
    value.setupCatalog = {
      version: 2,
      providers: [{
        id: "openai-compatible",
        display_name: "OpenAI-compatible",
        protocol: "openai_chat",
        requires_api_key: false,
        custom: true
      }]
    };
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.change(screen.getByLabelText("Provider"), {
      target: {value: "openai-compatible"}
    });
    fireEvent.change(screen.getByLabelText("Base URL"), {
      target: {value: "https://models.example.com/v1"}
    });
    fireEvent.change(screen.getByLabelText("Model ID"), {
      target: {value: "vendor-model"}
    });
    fireEvent.click(screen.getByRole("button", {name: "Detect model"}));
    expect(await screen.findByLabelText("Reasoning efforts")).toBeTruthy();
    expect(screen.getByLabelText("Default reasoning effort")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Start QCode"}));

    await waitFor(() => {
      expect(client.completeSetup).toHaveBeenCalledWith({
        provider: "openai-compatible",
        model: "vendor-model",
        api_key: "",
        base_url: "https://models.example.com/v1",
        protocol: "openai_chat",
        model_metadata: {
          canonical_id: "vendor-model",
          wire_id: "vendor-model",
          context_tokens: 200000,
          max_output_tokens: 24000,
          capabilities: expect.objectContaining({
            streaming: true,
            tool_calls: true,
            reasoning: true,
            reasoning_efforts: [],
            default_reasoning_effort: undefined
          })
        }
      });
    });
  });

  it.each([
    {settings: false, custom: true, protocol: "openai_responses", fail: false},
    {settings: true, custom: true, protocol: "openai_responses", fail: false},
    {settings: false, custom: false, protocol: "openai_chat", fail: false},
    {settings: true, custom: false, protocol: "openai_chat", fail: false},
    {settings: false, custom: false, protocol: "legacy", fail: true},
    {settings: true, custom: true, protocol: "openai_responses", fail: true}
  ])("configures an unknown model: $settings / $custom / $protocol / failure=$fail", async ({
    settings, custom, protocol, fail
  }) => {
    const value = snapshot();
    const providerID = custom ? "openai-compatible" : "provider";
    value.setupCatalog = {version: 2, providers: [{
      id: providerID, display_name: "Provider", protocol, custom,
      requires_api_key: false, models: ["catalog-model"]
    }]};
    if (!settings) {
      value.phase = "setup";
      value.sessions = [];
      value.selectedSessionID = "";
      value.profile = undefined;
    }
    const client = mockClient(value);
    if (fail) vi.mocked(client.probeSetup).mockRejectedValue(new Error("probe unavailable"));
    render(<App client={client} />);
    if (settings) {
      fireEvent.click(screen.getByRole("button", {name: "Settings"}));
      await screen.findByRole("dialog", {name: "Settings"});
      fireEvent.click(screen.getByRole("button", {name: "Connection"}));
      await screen.findByText("https://models.example.com/v1");
      fireEvent.click(screen.getByRole("button", {name: "Change provider"}));
    }
    const label = (name: string) => settings ? ({
      Provider: "Connection provider", "Base URL": "Connection base URL",
      Protocol: "Connection protocol", "Model ID": "Connection model ID"
    }[name]!) : name;
    fireEvent.change(screen.getByLabelText(label("Provider")), {target: {value: providerID}});
    if (custom) {
      fireEvent.change(screen.getByLabelText(label("Base URL")), {target: {value: "https://models.example.com/v1"}});
      fireEvent.change(screen.getByLabelText(label("Protocol")), {target: {value: protocol}});
    }
    fireEvent.change(screen.getByLabelText(label("Model ID")), {target: {value: "vendor-model"}});
    fireEvent.click(screen.getByRole("button", {name: "Detect model"}));
    await waitFor(() => expect(client.probeSetup).toHaveBeenCalledWith({
      provider: providerID, model: "vendor-model",
      base_url: custom ? "https://models.example.com/v1" : "", protocol
    }));
    const submitLabel = settings ? "Apply and restart" : "Start QCode";
    if (fail) {
      await screen.findByText("probe unavailable");
      fireEvent.click(screen.getByRole("button", {name: "Enter model metadata"}));
      expect(screen.getByRole("button", {name: submitLabel})).toHaveProperty("disabled", true);
      fireEvent.change(screen.getByLabelText("Context tokens"), {target: {value: "65536"}});
      fireEvent.change(screen.getByLabelText("Max output tokens"), {target: {value: "8192"}});
      fireEvent.click(screen.getByLabelText("Tool calling"));
    } else {
      await screen.findByLabelText("Detected model limits");
    }
    fireEvent.click(screen.getByRole("button", {name: submitLabel}));
    await waitFor(() => expect(client.completeSetup).toHaveBeenCalledWith(expect.objectContaining({
      provider: providerID, model: "vendor-model",
      ...(custom ? {protocol} : {}),
      model_metadata: expect.objectContaining({
        context_tokens: fail ? 65536 : 200000,
        max_output_tokens: fail ? 8192 : 24000,
        capabilities: expect.objectContaining({tool_calls: true})
      })
    })));
  });

  it("requests explicit discard when deleting an active session", () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const value = snapshot();
    value.sessions = [{
      ...value.sessions[0],
      status: "awaiting_approval",
      pending_approvals: 1
    }];
    const client = mockClient(value);
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Session actions for Chat"}));
    fireEvent.click(screen.getByRole("menuitem", {name: "Delete"}));

    expect(window.confirm).toHaveBeenCalledWith(
      'Delete "Chat" and permanently discard its unfinished work?'
    );
    expect(client.deleteSession).toHaveBeenCalledWith("session", 1, true);
  });

  it("warns that deleting an idle session discards a leftover workspace draft", () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const client = mockClient(snapshot());
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Session actions for Chat"}));
    fireEvent.click(screen.getByRole("menuitem", {name: "Delete"}));

    expect(window.confirm).toHaveBeenCalledWith(
      'Delete "Chat" and permanently discard its unfinished workspace draft if one exists?'
    );
    expect(client.deleteSession).toHaveBeenCalledWith("session", 1, true);
  });

  it("shows session lifecycle failures next to the affected row", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const client = mockClient(snapshot());
    vi.mocked(client.deleteSession).mockRejectedValue(
      new Error("cannot delete session while its isolated worktree has unresolved changes")
    );
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Session actions for Chat"}));
    fireEvent.click(screen.getByRole("menuitem", {name: "Delete"}));

    expect((await screen.findByRole("alert")).textContent).toContain(
      "cannot delete session while its isolated worktree has unresolved changes"
    );
    expect(client.deleteSession).toHaveBeenCalledWith("session", 1, true);
  });

  it("renders detailed activity artifacts and extension controls", async () => {
    const value = snapshot();
    value.agents = [{
      id: "agent-1",
      role: "reviewer",
      status: "working",
      last_message: "reviewing diff"
    }];
    value.usage = {
      turns: 2,
      calls: 3,
      total_tokens: 144,
      cost_microunits: 0,
      cost_known: false
    };
    value.plan = {
      version: 2,
      id: "plan-1",
      session_id: "session",
      thread_id: "thread",
      turn_id: "turn",
      cursor: 4,
      status: "ready",
      body: `{"version":1,"revision":1,"steps":[{"id":"implement",` +
        `"title":"Implement the verified change","status":"pending"}]}`,
      document: {
        version: 1,
        revision: 1,
        steps: [{
          id: "implement",
          title: "Implement the verified change",
          status: "pending"
        }]
      },
      profile_revision: 1,
      can_implement: true,
      can_autopilot: false,
      created_at: "2026-01-01T00:00:00Z"
    };
    value.checkpoints = [{
      version: 2,
      id: "checkpoint-1",
      session_id: "session",
      thread_id: "thread",
      turn_id: "turn",
      cursor: 3,
      status: "completed",
      summary: "Before implementation",
      profile_revision: 1,
      changed_files: 1,
      external_side_effects: false,
      can_restore: true,
      can_fork: true,
      created_at: "2026-01-01T00:00:00Z"
    }];
    value.extensions = [{
      kind: "skill",
      name: "review",
      enabled: true,
      health: "ready"
    }];
    const client = mockClient(value);
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Agent preset"}));
    expect(screen.getByLabelText("Usage").textContent).toContain(
      "Turns2Calls3Tokens144"
    );
    expect(screen.getAllByText("reviewing diff").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("Implement the verified change")).toBeTruthy();
    expect(screen.getByText("Before implementation")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", {name: "Restore"}));
    fireEvent.click(screen.getByRole("button", {name: "Fork"}));
    fireEvent.click(screen.getByRole("button", {name: "Extensions"}));
    fireEvent.click(screen.getByRole("checkbox", {name: /review/}));

    expect(client.restoreCheckpoint).toHaveBeenCalledWith("checkpoint-1");
    expect(client.forkCheckpoint).toHaveBeenCalledWith("checkpoint-1");
    expect(client.setExtensionEnabled).toHaveBeenCalledWith(
      "skill",
      "review",
      false
    );
  });

  it("discards staged Agent settings on close and applies one profile patch", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Agent preset"}));
    await waitFor(() => expect(client.listAgentPresets).toHaveBeenCalled());

    fireEvent.change(screen.getByLabelText("Agent mode"), {
      target: {value: "plan"}
    });
    expect(screen.queryByLabelText("Planning policy")).toBeNull();
    fireEvent.change(screen.getByLabelText("Maximum steps"), {
      target: {value: "16"}
    });
    expect(client.updateProfile).not.toHaveBeenCalled();
    expect(screen.getByText("Unsaved changes")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", {name: "Close settings"}));
    expect(screen.queryByRole("dialog", {name: "Settings"})).toBeNull();
    expect(client.updateProfile).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Agent preset"}));
    fireEvent.change(screen.getByLabelText("Agent mode"), {
      target: {value: "plan"}
    });
    fireEvent.change(screen.getByLabelText("Maximum steps"), {
      target: {value: "16"}
    });
    fireEvent.click(screen.getByRole("button", {name: "Apply changes"}));

    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({
        mode: "plan",
        max_steps: 16
      });
    });
    expect(await screen.findByText("Applied")).toBeTruthy();
  });

  it("stages tool allowlist changes and exposes guard metadata", async () => {
    const value = snapshot();
    value.tools = [
      ...value.tools,
      {
        ...value.tools[0]!,
        id: "write",
        name: "write_file",
        description: "Write a file",
        capability: "write",
        access_mode: "write",
        risk_level: "medium"
      }
    ];
    value.profile!.profile.enabled_tool_ids = ["read", "write"];
    const client = mockClient(value);
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Tools"}));

    fireEvent.click(screen.getByRole("checkbox", {name: "Disable read_file"}));
    expect(client.updateProfile).not.toHaveBeenCalled();
    fireEvent.click(screen.getAllByText("Details")[0]!);
    expect(screen.getAllByText("strong")).toHaveLength(2);
    expect(screen.getAllByText(/Validated when invoked/).length)
      .toBeGreaterThanOrEqual(2);

    fireEvent.click(screen.getByRole("button", {name: "Apply changes"}));
    await waitFor(() => {
      expect(client.updateProfile).toHaveBeenCalledWith({
        enabled_tool_ids: ["write"]
      });
    });
  });

  it("creates, edits, copies, applies, and deletes workspace Agent presets", async () => {
    const value = snapshot();
    const preset = {
      version: 1,
      id: "preset-review",
      revision: 3,
      name: "Review",
      description: "Review changes",
      scope: "workspace" as const,
      profile: {
        mode: "plan" as const,
        provider: "fixture",
        model: "fixture",
        reasoning_effort: "",
        enabled_tool_ids: ["builtin:read"],
        approval_posture: "suggest",
        execution_target: "local",
        max_steps: 16
      },
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z"
    };
    const client = mockClient(value);
    vi.mocked(client.listAgentPresets).mockResolvedValue({
      version: 1,
      revision: 3,
      presets: [preset]
    });
    vi.mocked(client.saveAgentPreset)
      .mockResolvedValueOnce({
        version: 1,
        revision: 4,
        preset: {...preset, revision: 4, name: "Strict review"}
      })
      .mockResolvedValueOnce({
        version: 1,
        revision: 5,
        preset: {
          ...preset,
          id: "preset-review-copy",
          revision: 1,
          name: "Strict review copy"
        }
      });
    vi.mocked(client.applyAgentPreset).mockResolvedValue({
      version: 1,
      preset_id: preset.id,
      profile_update: {
        profile: {
          ...value.profile!.profile,
          revision: 2,
          mode: "plan",
          max_steps: 16
        },
        prompt_cache_reset: true,
        reset_reason: "mode"
      },
      restart_required: false
    });
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Agent preset"}));
    await screen.findByDisplayValue("Review");

    fireEvent.click(screen.getByRole("button", {name: "Load into draft"}));
    expect(screen.getByText("Unsaved changes")).toBeTruthy();
    const applyPreset = screen.getByRole("button", {name: "Apply to session"});
    await waitFor(() => expect(applyPreset).toHaveProperty("disabled", false));
    fireEvent.click(applyPreset);
    await waitFor(() => {
      expect(client.applyAgentPreset).toHaveBeenCalledWith("preset-review");
    });

    fireEvent.change(screen.getByLabelText("Agent preset name"), {
      target: {value: "Strict review"}
    });
    fireEvent.click(screen.getByRole("button", {name: "Update"}));
    await waitFor(() => {
      expect(client.saveAgentPreset).toHaveBeenCalledWith(expect.objectContaining({
        id: "preset-review",
        expectedRevision: 3,
        name: "Strict review"
      }));
    });

    fireEvent.click(screen.getByRole("button", {name: "Duplicate"}));
    await waitFor(() => {
      expect(client.saveAgentPreset).toHaveBeenCalledWith(expect.objectContaining({
        name: "Strict review copy",
        profile: expect.objectContaining({mode: "plan", max_steps: 16})
      }));
    });

    fireEvent.click(screen.getByRole("button", {name: "Delete"}));
    fireEvent.click(screen.getByRole("button", {name: "Confirm delete"}));
    await waitFor(() => {
      expect(client.deleteAgentPreset).toHaveBeenCalledWith(
        expect.objectContaining({
          id: "preset-review-copy",
          revision: 1,
          name: "Strict review copy"
        })
      );
    });
  });

  it("shows skill metadata and routes diagnostics through control plane", async () => {
    const value = snapshot();
    value.extensions = [{
      kind: "skill",
      name: "review",
      version: "1.2.0",
      source: "workspace",
      trust: "catalog",
      digest: "a".repeat(64),
      enabled: true,
      health: "ready",
      permissions: ["workspace.read"]
    }];
    const client = mockClient(value);
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Extensions"}));

    expect(screen.getByText("Trust: catalog")).toBeTruthy();
    expect(screen.getByText("Permissions: workspace.read")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Details"}));
    await waitFor(() => {
      expect(client.controlExtension).toHaveBeenCalledWith(
        "skill",
        "review",
        "detail"
      );
    });
    expect(await screen.findByText(/"status": "ready"/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", {name: "Verify"}));
    await waitFor(() => {
      expect(client.controlExtension).toHaveBeenCalledWith(
        "skill",
        "review",
        "verify"
      );
    });
  });

  it("adds a server-issued workspace diff to prompt context", async () => {
    const client = mockClient(snapshot());
    const diff = {
      session_id: "session",
      thread_id: "thread",
      diff: "diff --git a/main.go b/main.go\n",
      digest: "b".repeat(64)
    };
    vi.mocked(client.workspaceDiff).mockResolvedValue(diff);
    render(<App client={client} />);
    await openContextDetails();

    fireEvent.click(screen.getByRole("button", {name: /Changes/}));
    await screen.findByText(/diff --git/);
    fireEvent.click(screen.getByRole("button", {name: "Add diff"}));

    expect(client.addGitDiffContext).toHaveBeenCalledWith(diff);
  });

  it("selects server-issued symbol and diagnostic contexts", async () => {
    const client = mockClient(snapshot());
    const symbol = {
      path: "main.go",
      name: "Serve",
      kind: "function",
      line: 3,
      uri: "file:///workspace/main.go",
      document_version: 1,
      digest: "a".repeat(64),
      range: {
        start: {line: 2, character: 0},
        end: {line: 2, character: 15}
      },
      selection_range: {
        start: {line: 2, character: 5},
        end: {line: 2, character: 10}
      }
    };
    const diagnostic = {
      call_id: "call-1",
      tool: "exec_command",
      status: "failed",
      context: {
        kind: "diagnostics" as const,
        source: "code_action" as const,
        uri: "file:///workspace/main.go",
        path: "main.go",
        document_version: 1,
        digest: "a".repeat(64),
        diagnostics: [{
          range: {
            start: {line: 2, character: 0},
            end: {line: 2, character: 6}
          },
          severity: "error" as const,
          message: "broken"
        }],
        explicit: true as const
      }
    };
    vi.mocked(client.searchWorkspaceSymbols).mockResolvedValue({
      query: "Serve",
      status: "ready",
      symbols: [symbol]
    });
    vi.mocked(client.workspaceDiagnostics).mockResolvedValue({
      session_id: "session",
      thread_id: "thread",
      diagnostics: [diagnostic]
    });
    render(<App client={client} />);
    await openContextDetails();

    fireEvent.change(screen.getByLabelText("Search workspace symbols"), {
      target: {value: "Serve"}
    });
    fireEvent.submit(screen.getByLabelText("Search workspace symbols").closest("form")!);
    fireEvent.click(await screen.findByRole("button", {name: /Serve/}));
    expect(client.addSymbolContext).toHaveBeenCalledWith(symbol);

    fireEvent.click(screen.getByRole("button", {name: "Diagnostics"}));
    fireEvent.click(await screen.findByRole("button", {name: /main.go.*failed/}));
    expect(client.addDiagnosticsContext).toHaveBeenCalledWith(diagnostic);
  });

  it("previews and selects a validated workspace image", async () => {
    const client = mockClient(snapshot());
    const image = {
      path: "diagram.png",
      uri: "file:///workspace/diagram.png",
      document_version: 1,
      digest: "c".repeat(64),
      bytes: 16,
      media_type: "image/png" as const,
      label: "diagram.png",
      content_handle: "signed-image-handle"
    };
    vi.mocked(client.browseWorkspace).mockResolvedValue({
      path: ".",
      entries: [{path: "diagram.png", kind: "file", size: 16}],
      more: false
    });
    vi.mocked(client.readWorkspaceImage).mockResolvedValue(image);
    vi.mocked(client.downloadWorkspaceContent).mockResolvedValue(
      new Blob(["image"], {type: "image/png"})
    );
    render(<App client={client} />);
    await openContextDetails();

    fireEvent.click(await screen.findByRole("button", {name: /diagram.png/}));
    expect(
      (await screen.findByRole("img", {name: "diagram.png"})).getAttribute("src")
    ).toBe("blob:workspace-resource");
    fireEvent.click(screen.getByRole("button", {name: "Add image"}));
    expect(client.addImageContext).toHaveBeenCalledWith(image);
  });

  it("adds only a completed tool result to prompt context", async () => {
    const value = snapshot();
    value.events = [
      event(1, "tool.start", {
        call_id: "call-1",
        tool: "exec_command",
        arguments: {cmd: "go test ./..."}
      }),
      event(2, "tool.result", {
        call_id: "call-1",
        tool: "exec_command",
        output: "ok",
        is_error: false
      })
    ];
    value.conversation = projectConversation(value.events);
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: /Bash go test/}));
    fireEvent.click(screen.getByRole("button", {name: "Add output"}));

    await waitFor(() => {
      expect(client.addTerminalContext).toHaveBeenCalledWith("call-1", "ok");
    });
  });

  it("keeps credential values in the keyring control flow", async () => {
    const status = {
      reference: {kind: "keyring", name: "qcode"},
      configured: true,
      validation: "valid" as const,
      validated_at: "2026-01-01T00:00:00Z",
      restart_required: true
    };
    const client = mockClient(snapshot());
    vi.mocked(client.credentialStatus).mockResolvedValue(status);
    vi.mocked(client.setKeyringCredential).mockResolvedValue(status);
    vi.mocked(client.validateCredential).mockResolvedValue(status);
    vi.mocked(client.clearKeyringCredential).mockResolvedValue({
      ...status,
      configured: false
    });
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Connection"}));
    await screen.findByText("valid");
    expect(screen.getByText("Runtime restart required")).toBeTruthy();
    expect(screen.getByText("Reference: qcode")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Provider credential"), {
      target: {value: "fixture-credential"}
    });
    fireEvent.click(screen.getByRole("button", {name: "Rotate key"}));
    await waitFor(() => {
      expect(client.setKeyringCredential).toHaveBeenCalledWith("fixture-credential");
    });
    const validate = screen.getByRole("button", {name: "Test connection"});
    await waitFor(() => expect(validate).toHaveProperty("disabled", false));
    fireEvent.click(validate);
    await waitFor(() => expect(client.validateCredential).toHaveBeenCalledOnce());
    const clear = screen.getByRole("button", {name: "Clear key"});
    await waitFor(() => expect(clear).toHaveProperty("disabled", false));
    fireEvent.click(clear);
    fireEvent.click(screen.getByRole("button", {name: "Confirm clear"}));
    expect(client.validateCredential).toHaveBeenCalledOnce();
    await waitFor(() => expect(client.clearKeyringCredential).toHaveBeenCalledOnce());
  });

  it("reconfigures the Runtime provider from Connection settings", async () => {
    const value = snapshot();
    value.setupCatalog = {
      version: 2,
      providers: [{
        id: "fixture",
        display_name: "Fixture",
        protocol: "openai_chat",
        requires_api_key: false
      }, {
        id: "deepseek",
        display_name: "DeepSeek",
        protocol: "openai_chat",
        requires_api_key: true,
        models: ["deepseek-chat"]
      }]
    };
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Settings"}));
    await screen.findByRole("dialog", {name: "Settings"});
    fireEvent.click(screen.getByRole("button", {name: "Connection"}));
    await screen.findByText("https://models.example.com/v1");
    fireEvent.click(screen.getByRole("button", {name: "Change provider"}));
    fireEvent.change(screen.getByLabelText("Connection provider"), {
      target: {value: "deepseek"}
    });
    fireEvent.change(screen.getByLabelText("Connection model ID"), {
      target: {value: "deepseek-chat"}
    });
    fireEvent.change(screen.getByLabelText("Connection API key"), {
      target: {value: "sk-next"}
    });
    fireEvent.click(screen.getByRole("button", {name: "Apply and restart"}));

    await waitFor(() => {
      expect(client.completeSetup).toHaveBeenCalledWith({
        provider: "deepseek",
        model: "deepseek-chat",
        api_key: "sk-next"
      });
    });
    expect(screen.queryByRole("dialog", {name: "Settings"})).toBeNull();
  });

  it("renders full approval decisions and structured input options", () => {
    const approval = event(1, "approval.required", {
      request_id: "approval",
      tool: "write_file",
      effect: "workspace write",
      allowed_scopes: ["once", "session"],
      replacement_allowed: true,
      edit_plan: {id: "a".repeat(64)}
    });
    const client = mockClient(snapshot([approval]));
    const approvalView = render(<App client={client} />);
    expect(screen.getByRole("button", {name: "Deny"})).toBeTruthy();
    const approveOnce = screen.getByRole("button", {name: "Approve once"});
    const approveSession = screen.getByRole("button", {name: "Approve for session"});
    fireEvent.click(screen.getByText("Approval options"));
    expect(screen.queryByLabelText("Replacement arguments")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Edit arguments"}));
    const replacement = screen.getByLabelText("Replacement arguments");
    fireEvent.change(replacement, {target: {value: "{"}});
    expect((approveOnce as HTMLButtonElement).disabled).toBe(true);
    expect((approveSession as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(replacement, {target: {value: "{\"path\":\"safe\"}"}});
    fireEvent.click(approveSession);
    expect(client.decideApproval).toHaveBeenCalledWith(
      "approval",
      "approve",
      "a".repeat(64),
      "session",
      {path: "safe"}
    );
    approvalView.unmount();

    const inputClient = mockClient(snapshot([
      event(2, "input.required", {
        request_id: "input",
        prompt: "Choose",
        options: ["one", "two"]
      })
    ]));
    render(<App client={inputClient} />);
    fireEvent.click(screen.getByRole("button", {name: "two"}));
    fireEvent.click(screen.getByRole("button", {name: "Submit"}));
    expect(inputClient.replyInput).toHaveBeenCalledWith(
      "input",
      "two",
      {selection: "two"}
    );
  });

  it("shows a file edit plan before approval and retains it on the tool row", () => {
    const value = snapshot([
      event(1, "tool.start", {
        call_id: "edit-1",
        tool: "file_edit",
        arguments: {
          path: "config.ts",
          old: "const enabled = false;\n",
          new: "const enabled = true;\n"
        }
      }),
      event(2, "approval.required", {
        request_id: "approval-edit",
        call_id: "edit-1",
        tool: "file_edit",
        effect: "workspace write",
        edit_plan: {
          id: "plan-edit",
          diff: "--- a/config.ts\n+++ b/config.ts\n-const enabled = false;\n+const enabled = true;\n",
          files: [{
            path: "config.ts",
            kind: "modified",
            before: "const enabled = false;\n",
            after: "const enabled = true;\n",
            before_exists: true,
            after_exists: true
          }]
        }
      })
    ]);
    const client = mockClient(value);
    const view = render(<App client={client} />);

    expect(screen.getByText("Review 1 file change")).toBeTruthy();
    expect(screen.getByText("const enabled = false;")).toBeTruthy();
    expect(screen.getByText("const enabled = true;")).toBeTruthy();
    view.unmount();

    value.events = [
      ...value.events,
      event(3, "approval.resolved", {
        request_id: "approval-edit",
        decision: "approve"
      }),
      event(4, "tool.result", {
        call_id: "edit-1",
        tool: "file_edit",
        output: "modified config.ts +1 -1",
        changes: [{path: "config.ts", kind: "modified", added: 1, removed: 1}],
        is_error: false
      })
    ];
    value.conversation = projectConversation(value.events);
    const completedView = render(<App client={mockClient(value)} />);
    const edit = screen.getByRole("button", {name: "Edit config.ts · +1 -1"});
    fireEvent.click(edit);

    expect(completedView.container.querySelector(".diffFooter")?.textContent)
      .toBe("+1 -1 · 1 file");
    expect(screen.getByText("const enabled = true;")).toBeTruthy();
    expect(screen.queryByRole("region", {name: "Produced files"})).toBeNull();
  });

  it("resets approval submission state for a new request id", async () => {
    const firstClient = mockClient(snapshot([
      event(1, "approval.required", {
        request_id: "approval-1",
        tool: "write_file"
      })
    ]));
    vi.mocked(firstClient.decideApproval).mockImplementation(
      () => new Promise(() => {})
    );
    const view = render(<App client={firstClient} />);

    fireEvent.click(screen.getByRole("button", {name: "Approve once"}));
    expect(screen.getByRole("button", {name: "Approve once"}))
      .toHaveProperty("disabled", true);

    const secondClient = mockClient(snapshot([
      event(2, "approval.required", {
        request_id: "approval-2",
        tool: "exec_command"
      })
    ]));
    view.rerender(<App client={secondClient} />);

    await waitFor(() => {
      expect(screen.getByRole("button", {name: "Approve once"}))
        .toHaveProperty("disabled", false);
    });
  });

  it("resets input submission state for a new request id", async () => {
    const firstClient = mockClient(snapshot([
      event(1, "input.required", {
        request_id: "input-1",
        prompt: "First input"
      })
    ]));
    vi.mocked(firstClient.replyInput).mockImplementation(
      () => new Promise(() => {})
    );
    const view = render(<App client={firstClient} />);
    const firstInput = screen.getByLabelText("Custom input answer");
    fireEvent.change(firstInput, {target: {value: "first"}});
    fireEvent.click(screen.getByRole("button", {name: "Submit"}));
    expect(screen.getByRole("button", {name: "Submit"}))
      .toHaveProperty("disabled", true);

    const secondClient = mockClient(snapshot([
      event(2, "input.required", {
        request_id: "input-2",
        prompt: "Second input"
      })
    ]));
    view.rerender(<App client={secondClient} />);

    await waitFor(() => {
      expect(screen.getByLabelText("Custom input answer"))
        .toHaveProperty("value", "");
      expect(screen.getByRole("button", {name: "Submit"}))
        .toHaveProperty("disabled", true);
    });
    fireEvent.change(screen.getByLabelText("Custom input answer"), {
      target: {value: "second"}
    });
    expect(screen.getByRole("button", {name: "Submit"}))
      .toHaveProperty("disabled", false);
  });

  it("restores and persists the selected Session draft", async () => {
    const client = mockClient(snapshot());
    vi.mocked(client.loadDraft).mockReturnValue("restored draft");
    render(<App client={client} />);

    const composer = await screen.findByDisplayValue("restored draft");
    fireEvent.change(composer, {target: {value: "edited draft"}});
    expect(client.saveDraft).toHaveBeenCalledWith("edited draft", "session");
  });

  it.each([
    {delay: 0, crossWorkspace: false},
    {delay: 149, crossWorkspace: false},
    {delay: 0, crossWorkspace: true},
    {delay: 149, crossWorkspace: true}
  ])("preserves drafts when switching after $delay ms (cross Workspace: $crossWorkspace)", async ({
    delay, crossWorkspace
  }) => {
    const value = snapshot();
    const client = mockClient(value);
    const drafts = new Map<string, string>();
    const key = (sessionID: string) => `${value.selectedWorkspaceID}:${sessionID}`;
    vi.mocked(client.loadDraft).mockImplementation((sessionID = value.selectedSessionID) =>
      drafts.get(key(sessionID)) ?? "");
    vi.mocked(client.saveDraft).mockImplementation((text, sessionID = value.selectedSessionID) => {
      drafts.set(key(sessionID), text);
    });
    const secondWorkspaceID = crossWorkspace ? "workspace-b" : "workspace-id";
    const secondSessionID = "session-b";
    value.sessions = [...value.sessions, {
      ...value.sessions[0]!,
      session_id: secondSessionID,
      title: "Second session",
      workspace_root: crossWorkspace ? "/workspace-b" : "/workspace"
    }];
    if (crossWorkspace) {
      value.workspaces = [...value.workspaces, {
        ...value.workspaces[0]!, id: secondWorkspaceID, root: "/workspace-b"
      }];
    }
    const view = render(<App client={client} />);
    const firstDraft = "  第一份草稿\n末尾空格 🙂  ";
    const secondDraft = "另一份\n独立草稿";
    vi.useFakeTimers();
    try {
      const switchTo = (workspaceID: string, sessionID: string) => {
        value.selectedWorkspaceID = workspaceID;
        value.selectedSessionID = sessionID;
        view.rerender(<App client={client} />);
      };
      fireEvent.change(screen.getByPlaceholderText("Ask QCode"), {
        target: {value: firstDraft}
      });
      await act(async () => { vi.advanceTimersByTime(delay); });
      switchTo(secondWorkspaceID, secondSessionID);
      expect(screen.getByPlaceholderText("Ask QCode")).toHaveProperty("value", "");
      fireEvent.change(screen.getByPlaceholderText("Ask QCode"), {
        target: {value: secondDraft}
      });
      await act(async () => { vi.advanceTimersByTime(delay); });
      switchTo("workspace-id", "session");
      expect(screen.getByPlaceholderText("Ask QCode")).toHaveProperty("value", firstDraft);
      switchTo(secondWorkspaceID, secondSessionID);
      expect(screen.getByPlaceholderText("Ask QCode")).toHaveProperty("value", secondDraft);
      view.unmount();
      expect(drafts.get("workspace-id:session")).toBe(firstDraft);
      expect(drafts.get(`${secondWorkspaceID}:${secondSessionID}`)).toBe(secondDraft);
      expect(drafts.size).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it.each([false, true])("keeps the saved draft until submission succeeds (failure: %s)", async (fails) => {
    const client = mockClient(snapshot());
    let savedDraft = "  待发送内容  ";
    vi.mocked(client.loadDraft).mockImplementation(() => savedDraft);
    vi.mocked(client.saveDraft).mockImplementation((text) => { savedDraft = text; });
    let resolve!: () => void;
    let reject!: (error: Error) => void;
    vi.mocked(client.submitPrompt).mockImplementation(() => new Promise((yes, no) => {
      resolve = () => yes({} as Awaited<ReturnType<RuntimeClient["submitPrompt"]>>);
      reject = no;
    }));
    render(<App client={client} />);
    fireEvent.click(screen.getByRole("button", {name: "Send"}));
    expect(client.submitPrompt).toHaveBeenCalledWith("待发送内容");
    expect(savedDraft).toBe("  待发送内容  ");
    await act(async () => {
      if (fails) reject(new Error("submission rejected"));
      else resolve();
    });
    expect(savedDraft).toBe(fails ? "  待发送内容  " : "");
    expect(screen.getByPlaceholderText("Ask QCode")).toHaveProperty("value", savedDraft);
  });

  it("preserves the textarea DOM node when a blank session becomes active", () => {
    const value = snapshot();
    const client = mockClient(value);
    const view = render(<App client={client} />);
    const textarea = screen.getByPlaceholderText("Ask QCode");

    value.events = [event(1, "turn.started", {display_prompt: "Hello"})];
    value.conversation = projectConversation(value.events);
    view.rerender(<App client={client} />);

    expect(screen.getByPlaceholderText("Ask QCode")).toBe(textarea);
    expect(screen.getByRole("button", {name: "Chat"})).toBeTruthy();
  });

  it("renders durable image inputs with the user message", () => {
    const value = snapshot([
      event(1, "turn.started", {
        display_prompt: "Describe this image",
        images: [{
          label: "lake.png",
          media_type: "image/png",
          content: "aW1hZ2U="
        }]
      })
    ]);
    render(<App client={mockClient(value)} />);

    const image = screen.getByRole("img", {name: "lake.png"});
    expect(image.getAttribute("src")).toBe("data:image/png;base64,aW1hZ2U=");
    expect(screen.getByText("Describe this image")).toBeTruthy();
  });

  it("steers an active turn from the composer while keeping stop available", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"})
    ]);
    const client = mockClient(value);
    render(<App client={client} />);

    const composer = await screen.findByPlaceholderText("Ask QCode");
    fireEvent.change(composer, {target: {value: "Focus on the parser"}});

    expect(screen.getByRole("button", {name: "Stop turn"})).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Steer current turn"}));

    await waitFor(() => {
      expect(client.steer).toHaveBeenCalledWith(
        "turn",
        "Focus on the parser"
      );
    });
    expect(client.submitPrompt).not.toHaveBeenCalled();
  });

  it("submits only one cancel while the active turn is stopping", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"})
    ]);
    const client = mockClient(value);
    vi.mocked(client.cancel).mockReturnValue(new Promise(() => {}));
    render(<App client={client} />);

    const stop = screen.getByRole("button", {name: "Stop turn"});
    fireEvent.click(stop);
    fireEvent.click(stop);

    expect(client.cancel).toHaveBeenCalledOnce();
    expect(stop.hasAttribute("disabled")).toBe(true);
  });

  it("single-flights stop while waiting for approval or input", () => {
    const pendingEvents = [
      event(1, "approval.required", {
        request_id: "approval",
        tool: "write_file",
        effect: "workspace write"
      }),
      event(1, "input.required", {
        request_id: "input",
        prompt: "Choose"
      })
    ];

    for (const pending of pendingEvents) {
      const client = mockClient(snapshot([pending]));
      vi.mocked(client.cancel).mockReturnValue(new Promise(() => {}));
      const view = render(<App client={client} />);

      const stop = screen.getByRole("button", {name: "Stop turn"});
      fireEvent.click(stop);
      fireEvent.click(stop);

      expect(client.cancel).toHaveBeenCalledOnce();
      expect(stop.hasAttribute("disabled")).toBe(true);
      view.unmount();
    }
  });

  it("keeps a visible paused state after a user interruption", () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "turn.canceled", {reason: "user_interrupted"})
    ]);
    value.sessions = value.sessions.map((session) => ({
      ...session,
      status: "interrupted"
    }));
    render(<App client={mockClient(value)} />);

    expect(screen.getByText("Paused", {selector: '[role="status"]'})).toBeTruthy();
    expect(document.title).toBe("(1) Paused · QCode");
    expect(screen.queryByRole("button", {name: "Stop turn"})).toBeNull();
  });

  it("stops active progress when the Runtime connection is interrupted", async () => {
    const value = {
      ...snapshot([event(1, "turn.started", {display_prompt: "Inspect"})]),
      phase: "failed" as const,
      socketConnected: false,
      problem: {
        version: 1,
        code: "internal",
        message: "Connection interrupted.",
        retryable: true
      }
    };
    render(<App client={mockClient(value)} />);

    expect(screen.getByRole("heading", {name: "Runtime unavailable"})).toBeTruthy();
    expect(screen.getByText("Connection interrupted.")).toBeTruthy();
    expect(document.querySelector(".turnStatus")).toBeNull();
    expect(screen.queryByText("Working")).toBeNull();
    expect(screen.queryByRole("button", {name: "Stop turn"})).toBeNull();
    expect(screen.queryByPlaceholderText("Ask QCode")).toBeNull();
  });

  it("keeps normal tool continuation in the status bar, not a chat card", () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "provider.attempt", {status: "completed", stop_reason: "tool_use"}),
      event(3, "tool.start", {call_id: "read", tool: "file_read", arguments: {path: "README.md"}}),
      event(4, "tool.result", {call_id: "read", tool: "file_read", output: "Read complete"})
    ]);
    render(<App client={mockClient(value)} />);
    expect(document.querySelector(".turnStatus")?.textContent).toContain("Continuing...");
    expect(screen.queryByText("Continuing this turn")).toBeNull();
    expect(screen.queryByText("Tool finished. Continuing the same turn.")).toBeNull();
  });

  it("queues Enter during an active turn and exposes queue item actions", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"})
    ]);
    value.queuedTurns = [{
      queue_id: "queue-1",
      thread_id: "thread",
      source_turn_id: "turn",
      prompt: "Run focused tests",
      added_sequence: 2,
      created_at: "2026-01-01T00:00:01Z",
      updated_at: "2026-01-01T00:00:01Z"
    }];
    const client = mockClient(value);
    render(<App client={client} />);

    const composer = await screen.findByPlaceholderText("Ask QCode");
    fireEvent.change(composer, {target: {value: "Check the parser"}});
    fireEvent.keyDown(composer, {key: "Enter"});
    await waitFor(() => {
      expect(client.enqueue).toHaveBeenCalledWith("turn", "Check the parser");
    });

    expect(await screen.findByText("1 queued message")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Edit queued message 1"}));
    const editor = screen.getByRole("textbox", {name: "Edit queued message 1"});
    fireEvent.change(editor, {target: {value: "Run all parser tests"}});
    fireEvent.click(screen.getByRole("button", {name: "Save queued message 1"}));
    await waitFor(() => {
      expect(client.updateQueuedTurn).toHaveBeenCalledWith(
        "queue-1",
        "Run all parser tests"
      );
    });
    fireEvent.click(screen.getByRole("button", {
      name: "Steer with queued message 1"
    }));
    await waitFor(() => {
      expect(client.promoteQueuedTurn).toHaveBeenCalledWith("queue-1", "turn");
    });
    fireEvent.click(screen.getByRole("button", {name: "Remove queued message 1"}));
    await waitFor(() => {
      expect(client.removeQueuedTurn).toHaveBeenCalledWith("queue-1");
    });
  });

  it("renders durable Think and opens a line-numbered Read card in the editor", () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "reasoning.completed", {
        sample_id: "sample-1",
        text: "Checking the repository\nReading the manifest"
      }),
      event(3, "tool.start", {
        call_id: "call",
        tool: "file_read",
        arguments: {path: "README.md", start_line: 41}
      }),
      event(4, "tool.result", {
        call_id: "call",
        tool: "file_read",
        output: "first line\nsecond line",
        is_error: false
      })
    ]);
    value.sessions = value.sessions.map((session) => ({
      ...session,
      status: "completed"
    }));
    const client = mockClient(value);
    const {container} = render(<App client={client} />);

    expect(container.querySelectorAll(".disclosure pre")).toHaveLength(0);
    expect(screen.getByText("README.md")).toBeTruthy();
    expect(screen.getByText("Checking the repository")).toBeTruthy();
    expect(screen.getByText("Think")).toBeTruthy();
    expect(screen.queryByText("completed", {exact: true})).toBeNull();
    expect(container.querySelectorAll(".transcript .disclosureLeading")).toHaveLength(2);
    expect(container.querySelectorAll(".transcript .disclosureChevron")).toHaveLength(2);

    fireEvent.click(screen.getByRole("button", {name: /Read README\.md/}));
    expect(container.querySelector("[data-read]")).toBeTruthy();
    expect(screen.getByText("41")).toBeTruthy();
    expect(screen.getByText("first line")).toBeTruthy();
    expect(screen.queryByRole("button", {name: "README.md"})).toBeNull();
    expect(container.querySelector(".surfaceFileLabel")?.textContent).toBe("README.md");
  });

  it("collapses completed turn execution while keeping its final conclusion visible", () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "reasoning.completed", {
        sample_id: "sample-1",
        text: "Checking the repository"
      }),
      event(3, "tool.start", {
        call_id: "call",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(4, "tool.result", {
        call_id: "call",
        tool: "file_read",
        output: "# QCode",
        is_error: false
      }),
      event(5, "turn.completed", {text: "Repository inspected"})
    ]);
    render(<App client={mockClient(value)} />);

    expect(screen.getByText("Repository inspected")).toBeTruthy();
    expect(screen.queryByText("Checking the repository")).toBeNull();
    expect(screen.queryByRole("button", {name: /Read README\.md/})).toBeNull();

    const toggle = screen.getByRole("button", {name: /Execution details/});
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);

    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("Checking the repository")).toBeTruthy();
    expect(screen.getByRole("button", {name: /Read README\.md/})).toBeTruthy();
  });

  it("keeps commentary in completed execution and reveals it through conversation search", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "commentary.completed", {
        message_id: "message", sample_id: "sample", text: "Located the dispatch handler.",
        call_ids: ["call"]
      }),
      event(3, "tool.start", {call_id: "call", tool: "file_read", arguments: {path: "main.go"}}),
      event(4, "tool.result", {call_id: "call", output: "package main", is_error: false}),
      event(5, "turn.completed", {text: "Repository inspected"})
    ]);
    const {container} = render(<App client={mockClient(value)} />);
    expect(screen.queryByText("Located the dispatch handler.")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: /Execution details/}));
    expect(await screen.findByText("Located the dispatch handler.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: /Stage details/}));
    expect(screen.queryByRole("button", {name: /Read main.go/})).toBeNull();
    expect(screen.getByText("Repository inspected")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: /Execution details/}));
    fireEvent.click(screen.getByRole("button", {name: "Search conversation"}));
    fireEvent.change(await screen.findByRole("combobox", {name: "Search conversation"}), {
      target: {value: "dispatch handler"}
    });
    fireEvent.click(screen.getByRole("option", {name: /Located the dispatch handler/}));
    await waitFor(() => expect(container.querySelector(".commentaryMessage")?.textContent)
      .toContain("Located the dispatch handler."));
    expect(container.querySelectorAll(".commentaryMessage")).toHaveLength(1);
  });

  it("renders Bash output and grouped Grep results as dedicated cards", () => {
    const value = snapshot([
      event(1, "tool.start", {
        call_id: "bash",
        tool: "exec_command",
        arguments: {command: "go test ./...", cwd: "."}
      }),
      event(2, "tool.result", {
        call_id: "bash",
        tool: "exec_command",
        output: "ok example/project",
        changes: [{path: "build/test", kind: "modified", added: 0, removed: 0}],
        is_error: false
      }),
      event(3, "command.execution", {
        call_id: "bash",
        command: "go test ./...",
        status: "completed",
        exit_code: 0
      }),
      event(4, "tool.start", {
        call_id: "grep",
        tool: "search_text",
        arguments: {pattern: "^Serve", path: "server.go"}
      }),
      event(5, "tool.result", {
        call_id: "grep",
        tool: "search_text",
        output: JSON.stringify({
          matches: [
            {file: "server.go", line: 18, text: "func Serve() {}"},
            {file: "server.go", line: 31, text: "Serve()"}
          ],
          total: 2,
          truncated: false
        }),
        is_error: false
      })
    ]);
    const {container} = render(<App client={mockClient(value)} />);

    fireEvent.click(screen.getByRole("button", {name: /Bash go test/}));
    expect(container.querySelector("[data-terminal]")).toBeTruthy();
    expect(screen.getByText("ok example/project")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", {name: /Grep \^Serve · server\.go/}));
    expect(container.querySelector("[data-search='search-matches']")).toBeTruthy();
    expect(screen.getByText("2 matches · 1 files")).toBeTruthy();
    expect(screen.getByText("18:")).toBeTruthy();
  });

  it("shows the executed command instead of the exit code in a failed Bash summary", () => {
    const value = snapshot([
      event(1, "tool.start", {
        call_id: "bash-failed",
        tool: "exec_command",
        arguments: {command: "go test ./...", cwd: "."}
      }),
      event(2, "tool.result", {
        call_id: "bash-failed",
        tool: "exec_command",
        output: "package tests failed",
        is_error: true
      }),
      event(3, "command.execution", {
        call_id: "bash-failed",
        command: "go test ./...",
        status: "failed",
        exit_code: 1
      })
    ]);
    render(<App client={mockClient(value)} />);

    expect(screen.getByRole("button", {name: "Bash go test ./..."})).toBeTruthy();
    expect(screen.queryByRole("button", {name: "Bash exit 1"})).toBeNull();
  });

  it("shows file_write content in the expanded Write card", () => {
    const value = snapshot([
      event(1, "tool.start", {
        call_id: "write-content",
        tool: "file_write",
        arguments: {
          path: "generated.txt",
          content: "first line\nsecond line\n"
        }
      }),
      event(2, "tool.result", {
        call_id: "write-content",
        tool: "file_write",
        output: "written",
        changes: [{path: "generated.txt", kind: "created", added: 2, removed: 0}],
        is_error: false
      })
    ]);
    render(<App client={mockClient(value)} />);

    fireEvent.click(screen.getByRole("button", {
      name: "Write generated.txt · +2 -0"
    }));
    expect(screen.getByText("first line")).toBeTruthy();
    expect(screen.getByText("second line")).toBeTruthy();
  });

  it("shows structured recovery details beside a failed edit preview", () => {
    const output = "old text matched 0 times; required_action=file_read; " +
      "retry_original=false; path=src/main.cpp";
    const value = snapshot([
      event(1, "tool.start", {
        call_id: "edit",
        tool: "file_edit",
        arguments: {
          path: "src/main.cpp",
          old: "int old_value;",
          new: "int new_value;"
        }
      }),
      event(2, "tool.result", {
        call_id: "edit",
        tool: "file_edit",
        output,
        is_error: true,
        recovery: {
          error_category: "edit_precondition_miss",
          required_action: "file_read",
          path: "src/main.cpp",
          retry_original: false
        }
      })
    ]);
    const {container} = render(<App client={mockClient(value)} />);

    fireEvent.click(screen.getByRole("button", {name: /Edit old text matched/}));
    expect(container.querySelector(".diffCard")).toBeTruthy();
    expect(container.querySelector(".toolIOCard pre[data-error]")?.textContent).toBe(output);
  });

  it("disables Session-bound controls while the selected Session hydrates", () => {
    const value = snapshot([]);
    value.hydratingSessionID = value.selectedSessionID;
    render(<App client={mockClient(value)} />);

    expect(screen.getByText("Loading")).toBeTruthy();
    expect(screen.getByPlaceholderText("Ask QCode"))
      .toHaveProperty("disabled", true);
    expect(screen.getByRole("button", {name: "Attach files"}))
      .toHaveProperty("disabled", true);
    expect(screen.queryByRole("button", {name: "Export session"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Session inspector"})).toBeNull();
  });

  it("opens external Markdown links safely and gates remote images", async () => {
    const value = snapshot([
      event(1, "output.delta", {
        text: "[docs](https://example.com) ![remote](https://example.com/image.png)"
      })
    ]);
    const {container} = render(<App client={mockClient(value)} />);

    const link = await screen.findByRole("link", {name: "docs"});
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("button", {name: "Load image remote"})).toBeTruthy();
  });

  it("renders settled response actions and persists feedback through the client", async () => {
    const value = snapshot([
      {
        ...event(1, "turn.started", {display_prompt: "Summarize"}),
        created_at: "2026-01-01T00:00:00Z"
      },
      event(2, "turn.receipt", {
        latency: {
          total_ms: 29_000,
          provider_ms: 6_000,
          first_token_ms: 900
        },
        output_tokens: 826
      }),
      {
        ...event(3, "turn.completed", {text: "Final answer"}),
        created_at: "2026-01-01T00:00:29Z"
      }
    ]);
    const client = mockClient(value);
    render(<App client={client} />);

    expect(screen.getByText(/Ran for 29s.*0.9s TTFT.*~162 tok\/s/))
      .toBeTruthy();
    fireEvent.click(screen.getByRole("button", {name: "Copy response"}));
    await waitFor(() => {
      expect(clipboardWrite).toHaveBeenCalledWith("Final answer");
    });
    fireEvent.click(screen.getByRole("button", {name: "Like response"}));
    expect(client.toggleMessageFeedback).toHaveBeenCalledWith(
      "output-turn",
      "positive"
    );
  });

  it("opens the command menu and submits context compaction", async () => {
    const value = snapshot([
      event(1, "turn.completed", {text: "Done"})
    ]);
    value.sessions = value.sessions.map((session) => ({
      ...session,
      latest_turn_id: "turn"
    }));
    const client = mockClient(value);
    render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: "Commands"}));
    expect(screen.getByRole("menu", {name: "Commands"})).toBeTruthy();
    fireEvent.click(screen.getByRole("menuitem", {name: /compact/}));

    await waitFor(() => {
      expect(client.compactThread).toHaveBeenCalled();
    });
  });

  it("opens slash commands with search, argument hints, keyboard selection, and recents", async () => {
    const client = mockClient(snapshot());
    const inputClick = vi.spyOn(HTMLInputElement.prototype, "click");
    render(<App client={client} />);

    const composer = screen.getByPlaceholderText("Ask QCode");
    fireEvent.change(composer, {target: {value: "/att"}});

    let search = await screen.findByRole("searchbox", {name: "Search commands"});
    expect(search).toHaveProperty("value", "att");
    expect(screen.getByRole("menuitem", {name: /\/attach file/})).toBeTruthy();
    expect(screen.queryByRole("menuitem", {name: /\/compact/})).toBeNull();

    fireEvent.keyDown(search, {key: "Escape"});
    expect(composer).toHaveProperty("value", "");
    expect(document.activeElement).toBe(composer);

    fireEvent.change(composer, {target: {value: "/att"}});
    search = await screen.findByRole("searchbox", {name: "Search commands"});
    fireEvent.keyDown(search, {key: "ArrowDown"});
    fireEvent.keyDown(search, {key: "Enter"});
    expect(inputClick).toHaveBeenCalled();
    expect(composer).toHaveProperty("value", "");

    fireEvent.click(screen.getByRole("button", {name: "Commands"}));
    expect(screen.getByText("Recent")).toBeTruthy();
    expect(screen.getAllByRole("menuitem")[0]?.textContent).toContain("/attach");
    fireEvent.change(screen.getByRole("searchbox", {name: "Search commands"}), {
      target: {value: "missing-command"}
    });
    expect(screen.getByText("No matching commands")).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("searchbox", {name: "Search commands"}), {
      key: "Escape"
    });
    expect(screen.queryByRole("menu", {name: "Commands"})).toBeNull();
  });

  it("normalizes picker, paste, and drop files through one attachment pipeline", async () => {
    const value = snapshot();
    const client = mockClient(value);
    const {container} = render(<App client={client} />);
    const picker = container.querySelector<HTMLInputElement>(
      'input[type="file"][aria-label="Attach files"]'
    );
    const composer = screen.getByPlaceholderText("Ask QCode");
    const surface = container.querySelector<HTMLElement>(".composer");
    expect(picker).toBeTruthy();
    expect(surface).toBeTruthy();

    fireEvent.change(picker!, {
      target: {files: [fixtureFile("picker.txt", "text/plain", "picker")]}
    });
    expect(await screen.findByText("Text · 6 B · picker")).toBeTruthy();

    fireEvent.paste(composer, {
      clipboardData: {
        files: [fixtureFile("paste.md", "text/markdown", "paste")]
      }
    });
    expect(await screen.findByText("Text · 5 B · paste")).toBeTruthy();

    fireEvent.dragEnter(surface!, {
      dataTransfer: {types: ["Files"], files: []}
    });
    expect(surface?.getAttribute("data-dragging")).toBe("true");
    fireEvent.drop(surface!, {
      dataTransfer: {
        types: ["Files"],
        files: [fixtureFile("drop.json", "application/json", "{}")]
      }
    });
    expect(await screen.findByText("Text · 2 B · drop")).toBeTruthy();
    expect(client.addAttachmentContext).toHaveBeenCalledTimes(3);

    fireEvent.click(screen.getByRole("button", {
      name: "Remove attachment paste.md"
    }));
    expect(client.removeAttachmentContext).toHaveBeenCalledWith(
      expect.stringMatching(/^[0-9a-f]{64}$/)
    );
  });

  it("keeps failed attachments explicit and blocks accidental omission", async () => {
    const client = mockClient(snapshot());
    const {container} = render(<App client={client} />);
    const picker = container.querySelector<HTMLInputElement>(
      'input[type="file"][aria-label="Attach files"]'
    );
    fireEvent.change(picker!, {
      target: {
        files: [fixtureBytes("archive.zip", "application/zip", Uint8Array.of(1))]
      }
    });

    expect(await screen.findByText(/not a supported text or image attachment/))
      .toBeTruthy();
    const composer = screen.getByPlaceholderText("Ask QCode");
    fireEvent.change(composer, {target: {value: "Inspect this archive"}});
    expect(screen.getByRole("button", {name: "Send"}))
      .toHaveProperty("disabled", true);

    fireEvent.click(screen.getByRole("button", {
      name: "Remove attachment archive.zip"
    }));
    expect(screen.getByRole("button", {name: "Send"}))
      .toHaveProperty("disabled", false);
  });

  it("discards an attachment that finishes after switching Sessions", async () => {
    let resolveFile: ((value: ArrayBuffer) => void) | undefined;
    const delayed = {
      name: "delayed.txt",
      type: "text/plain",
      size: 7,
      arrayBuffer: vi.fn(() => new Promise<ArrayBuffer>((resolve) => {
        resolveFile = resolve;
      }))
    } as unknown as File;
    const value = snapshot();
    const client = mockClient(value);
    const view = render(<App client={client} />);
    const picker = view.container.querySelector<HTMLInputElement>(
      'input[type="file"][aria-label="Attach files"]'
    );

    fireEvent.change(picker!, {target: {files: [delayed]}});
    expect(screen.getByText("Processing · picker")).toBeTruthy();

    value.sessions = [
      ...value.sessions,
      {...value.sessions[0]!, session_id: "session-2", thread_id: "thread-2"}
    ];
    value.selectedSessionID = "session-2";
    view.rerender(<App client={client} />);
    resolveFile?.(new TextEncoder().encode("delayed").buffer);

    await waitFor(() => {
      expect(screen.queryByText("delayed.txt")).toBeNull();
    });
    expect(client.addAttachmentContext).not.toHaveBeenCalled();
  });

  it("does not submit while an IME composition is active", async () => {
    const client = mockClient(snapshot());
    render(<App client={client} />);
    const composer = screen.getByPlaceholderText("Ask QCode");
    fireEvent.change(composer, {target: {value: "检查解析器"}});
    fireEvent.compositionStart(composer);
    fireEvent.keyDown(composer, {key: "Enter"});
    expect(client.submitPrompt).not.toHaveBeenCalled();

    fireEvent.compositionEnd(composer);
    fireEvent.keyDown(composer, {key: "Enter"});
    await waitFor(() => {
      expect(client.submitPrompt).toHaveBeenCalledWith("检查解析器");
    });
  });

  it("keeps long drafts internally bounded and visible above mobile keyboards", async () => {
    const listeners = new Map<string, EventListener>();
    const previousViewport = window.visualViewport;
    const scrollIntoView = vi.fn();
    Object.defineProperty(window, "visualViewport", {
      configurable: true,
      value: {
        addEventListener: (type: string, listener: EventListener) => {
          listeners.set(type, listener);
        },
        removeEventListener: (type: string) => {
          listeners.delete(type);
        }
      }
    });
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: scrollIntoView
    });
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
      callback(0);
      return 1;
    });

    try {
      render(<App client={mockClient(snapshot())} />);
      const composer = screen.getByPlaceholderText(
        "Ask QCode"
      ) as HTMLTextAreaElement;
      Object.defineProperty(composer, "scrollHeight", {
        configurable: true,
        value: 900
      });
      fireEvent.change(composer, {target: {value: "line\n".repeat(300)}});
      expect(composer.style.height).toBe("336px");

      composer.focus();
      listeners.get("resize")?.(new Event("resize"));
      expect(scrollIntoView).toHaveBeenCalledWith({block: "nearest"});
    } finally {
      Object.defineProperty(window, "visualViewport", {
        configurable: true,
        value: previousViewport
      });
    }
  });

  it("shows provider-attributed context usage beside the send action", () => {
    const value = snapshot([
      event(1, "usage", {
        context: {
          estimated_tokens: 32_000,
          stable_tokens: 2_000,
          dynamic_tokens: 1_000,
          continuation_tokens: 500,
          tool_definition_tokens: 6_000,
          history_tool_tokens: 1_000,
          history_user_tokens: 8_000,
          history_assistant_tokens: 10_000,
          history_other_tokens: 500,
          provider_framing_tokens: 3_000
        }
      })
    ]);
    render(<App client={mockClient(value)} />);

    fireEvent.click(screen.getByRole("button", {name: "25% of context used"}));
    const panel = screen.getByRole("dialog", {name: "Context usage"});
    expect(panel.textContent).toContain("~32K / 128K");
    expect(panel.textContent).toContain("Stable / system~3.5K");
    expect(panel.textContent).toContain("Tools~7K");
    expect(panel.textContent).toContain("Messages~18.5K");
    expect(panel.textContent).toContain("Provider framing~3K");
  });

  it("renders GFM tables in a keyboard-scrollable Markdown wrapper", () => {
    const value = snapshot([
      event(1, "turn.completed", {
        text: [
          "## Core modules",
          "",
          "| Package | Role |",
          "| --- | --- |",
          "| runtime | Agent loop |"
        ].join("\n")
      })
    ]);
    const {container} = render(<App client={mockClient(value)} />);

    expect(screen.getByRole("heading", {name: "Core modules"})).toBeTruthy();
    expect(screen.getByRole("table")).toBeTruthy();
    expect(screen.getByRole("region", {name: "Response table"}).getAttribute("tabindex"))
      .toBe("0");
    expect(container.querySelector(".assistantMarkdown")).toBeTruthy();
  });

  it("opens the three-lane trajectory and inspects a tool from chat", async () => {
    const value = snapshot([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(3, "tool.result", {
        call_id: "call-1",
        tool: "file_read",
        output: "# Project",
        is_error: false
      }),
      event(4, "turn.completed", {text: "Done"})
    ]);
    value.tracePhase = "ready";
    value.trace = {
      version: 1,
      session_id: "session",
      through_sequence: 4,
      turns: [{
        turn_id: "turn",
        status: "ok",
        spans: [{
          id: 1,
          kind: "tool",
          status: "ok",
          started_at: "2026-01-01T00:00:02Z",
          ended_at: "2026-01-01T00:00:03Z",
          duration_ms: 1_000,
          call_id: "call-1"
        }]
      }]
    };
    const client = mockClient(value);
    const {container} = render(<App client={client} />);

    fireEvent.click(screen.getByRole("button", {name: /Execution details/}));
    fireEvent.click(screen.getByRole("button", {name: /Read README\.md/}));
    fireEvent.click(screen.getByRole("button", {name: "Inspect"}));

    const trajectory = await screen.findByLabelText("Execution trajectory");
    expect(trajectory.querySelector(".timelineLabels")?.textContent)
      .toBe("InputModelTools");
    const inspector = screen.getByLabelText("Record inspector");
    expect(inspector.textContent).toContain("call-1");
    expect(screen.getByRole("tab", {name: "Summary"}).getAttribute("aria-selected"))
      .toBe("true");
    fireEvent.click(screen.getByRole("tab", {name: "Input"}));
    expect(screen.getByRole("button", {name: "Copy input"})).toBeTruthy();
    expect(screen.getByRole("separator", {name: "Resize record inspector"}))
      .toBeTruthy();
    expect(client.refreshTrace).toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", {name: "Show in chat"}));
    await waitFor(() => {
      expect(screen.getByRole("button", {name: "Chat"}).getAttribute("aria-current"))
        .toBe("page");
      expect(
        container.querySelector("[data-entry-id='tool-call-1'][data-navigation-current]")
      ).toBeTruthy();
    });
  });

  it("searches stable conversation identities and navigates between questions", async () => {
    const events = [
      {...event(1, "turn.started", {display_prompt: "Inspect the parser"}), turn_id: "turn-a"},
      {...event(2, "tool.start", {
        call_id: "call-read",
        tool: "file_read",
        arguments: {path: "README.md"}
      }), turn_id: "turn-a"},
      {...event(3, "tool.result", {
        call_id: "call-read",
        tool: "file_read",
        output: "# Project",
        is_error: false
      }), turn_id: "turn-a"},
      {...event(4, "turn.completed", {text: "Parser inspected"}), turn_id: "turn-a"},
      {...event(5, "turn.started", {display_prompt: "Update the tests"}), turn_id: "turn-b"},
      {...event(6, "turn.completed", {text: "Tests updated"}), turn_id: "turn-b"},
      {...event(7, "turn.started", {display_prompt: "Verify the result"}), turn_id: "turn-c"},
      {...event(8, "turn.completed", {text: "Result verified"}), turn_id: "turn-c"}
    ];
    const {container} = render(<App client={mockClient(snapshot(events))} />);

    expect(screen.getByRole("button", {name: "Search conversation"}).textContent)
      .toContain("3/3");
    fireEvent.keyDown(document, {key: "f", ctrlKey: true});
    const dialog = await screen.findByRole("dialog", {name: "Search conversation"});
    fireEvent.change(screen.getByRole("combobox", {name: "Search conversation"}), {
      target: {value: "README.md"}
    });
    fireEvent.click(screen.getByRole("tab", {name: "Files"}));
    fireEvent.click(screen.getByRole("option", {name: /README\.md/}));

    await waitFor(() => {
      expect(dialog.isConnected).toBe(false);
      expect(
        container.querySelector("[data-entry-id='tool-call-read'][data-navigation-current]")
      ).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", {name: "Next user question"}));
    await waitFor(() => {
      expect(
        container.querySelector("[data-entry-id='event-5'][data-navigation-current]")
      ).toBeTruthy();
      expect(screen.getByRole("button", {name: "Search conversation"}).textContent)
        .toContain("2/3");
    });
    fireEvent.keyDown(document, {key: "ArrowDown", altKey: true});
    await waitFor(() => {
      expect(
        container.querySelector("[data-entry-id='event-7'][data-navigation-current]")
      ).toBeTruthy();
    });
  });

  it("windows 500-turn transcripts to 200 projected rows with automatic history scrolling", async () => {
    const events = Array.from({length: 500}, (_, index) => ({
      ...event(index + 1, "turn.completed", {
        text: `message-${index + 1}`,
        outcome: "answered"
      }),
      turn_id: `turn-${index + 1}`
    }));
    const {container} = render(<App client={mockClient(snapshot(events))} />);

    expect(container.querySelectorAll(".assistantMessage, .terminalState")).toHaveLength(200);
    expect(screen.queryByText("message-300")).toBeNull();
    expect(screen.getByText("message-301")).toBeTruthy();
    expect(screen.getByText("message-500")).toBeTruthy();

    const scrollport = mockTranscriptViewport(container);
    fireEvent.scroll(scrollport, {target: {scrollTop: 4_000}});
    fireEvent.scroll(scrollport, {target: {scrollTop: 0}});
    await waitFor(() => expect(screen.getByText("message-300")).toBeTruthy());
    expect(container.querySelectorAll(".assistantMessage, .terminalState")).toHaveLength(200);
    expect(screen.queryByText("message-500")).toBeNull();
    expect(screen.queryByRole("button", {name: "Earlier messages"})).toBeNull();
    expect(screen.queryByRole("button", {name: "Newer messages"})).toBeNull();
    const anchor = screen.getByText("message-301").closest<HTMLElement>("[data-entry-id]")!;
    expect(anchor.getBoundingClientRect().top).toBe(0);

    fireEvent.scroll(scrollport, {target: {scrollTop: scrollport.scrollHeight - scrollport.clientHeight}});
    await waitFor(() => expect(screen.getByText("message-500")).toBeTruthy());
    expect(container.querySelectorAll(".assistantMessage, .terminalState")).toHaveLength(200);
  });

  it("pins older entries during new output and returns to the latest window", async () => {
    const events = Array.from({length: 500}, (_, index) => ({
      ...event(index + 1, "turn.completed", {text: `message-${index + 1}`, outcome: "answered"}),
      turn_id: `turn-${index + 1}`
    }));
    const value = snapshot(events);
    const client = mockClient(value);
    const view = render(<App client={client} />);
    const scrollport = mockTranscriptViewport(view.container);
    fireEvent.scroll(scrollport, {target: {scrollTop: 4_000}});
    fireEvent.scroll(scrollport, {target: {scrollTop: 0}});
    await waitFor(() => expect(screen.getByText("message-300")).toBeTruthy());
    const top = scrollport.scrollTop;
    const newer = {...event(501, "turn.completed", {text: "new live answer"}), turn_id: "turn-501"};
    value.events = [...events, newer];
    value.conversation = projectConversation(value.events);
    view.rerender(<App client={client} />);
    expect(screen.getByText("message-300")).toBeTruthy();
    expect(screen.queryByText("new live answer")).toBeNull();
    expect(scrollport.scrollTop).toBe(top);
    fireEvent.click(screen.getByRole("button", {name: "Back to bottom"}));
    await waitFor(() => expect(screen.getByText("new live answer")).toBeTruthy());
    expect(view.container.querySelectorAll(".assistantMessage, .terminalState")).toHaveLength(200);
  });

  it("loads history once at the top and waits for explicit retry after failure", async () => {
    const events = Array.from({length: 40}, (_, index) => ({
      ...event(index + 101, "turn.completed", {text: `message-${index + 101}`}),
      turn_id: `turn-${index + 101}`
    }));
    const value = snapshot(events);
    value.historyMoreBefore = true;
    const client = mockClient(value);
    let rejectLoad!: (error: Error) => void;
    vi.mocked(client.loadEarlierHistory).mockImplementationOnce(() => new Promise<number>((_resolve, reject) => {
      rejectLoad = reject;
    }));
    const view = render(<App client={client} />);
    const scrollport = mockTranscriptViewport(view.container);
    fireEvent.scroll(scrollport, {target: {scrollTop: 800}});
    fireEvent.scroll(scrollport, {target: {scrollTop: 0}});
    await waitFor(() => expect(client.loadEarlierHistory).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("status", {name: "Loading earlier messages"})).toBeTruthy();
    fireEvent.scroll(scrollport);
    fireEvent.scroll(scrollport);
    await act(async () => rejectLoad(new Error("Offline")));
    expect(await screen.findByRole("button", {name: "Retry loading history"})).toBeTruthy();
    fireEvent.scroll(scrollport);
    expect(client.loadEarlierHistory).toHaveBeenCalledTimes(1);
    vi.mocked(client.loadEarlierHistory).mockImplementationOnce(async () => {
      value.historyMoreBefore = false;
      return 0;
    });
    fireEvent.click(screen.getByRole("button", {name: "Retry loading history"}));
    await waitFor(() => expect(client.loadEarlierHistory).toHaveBeenCalledTimes(2));
    expect(screen.queryByText("Offline")).toBeNull();
  });
});

function mockTranscriptViewport(container: HTMLElement): HTMLElement {
  const scrollport = container.querySelector<HTMLElement>("[data-conversation-scroll]")!;
  Object.defineProperty(scrollport, "clientHeight", {configurable: true, value: 400});
  Object.defineProperty(scrollport, "scrollHeight", {
    configurable: true,
    get: () => scrollport.querySelectorAll("[data-entry-id]").length * 40
  });
  let scrollTop = 0;
  Object.defineProperty(scrollport, "scrollTop", {
    configurable: true,
    get: () => scrollTop,
    set: (value: number) => { scrollTop = Math.max(0, Math.min(value, scrollport.scrollHeight - 400)); }
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    if (this === scrollport) return new DOMRect(0, 0, 800, 400);
    if (this.hasAttribute("data-composer-seat")) return new DOMRect(0, 350, 800, 50);
    const anchor = this.closest("[data-entry-id]");
    const index = Array.from(scrollport.querySelectorAll("[data-entry-id]")).indexOf(anchor!);
    return new DOMRect(0, index * 40 - scrollTop, 800, index >= 0 ? 40 : 0);
  });
  return scrollport;
}

function snapshot(events: RuntimeEvent[] = []): RuntimeSnapshot {
  const session: SessionSummary = {
    version: 1,
    revision: 1,
    session_id: "session",
    thread_id: "thread",
    title: "Chat",
    status: "idle",
    pinned: false,
    archived: false,
    isolation: "shared",
    workspace_root: "/workspace",
    workspace_label: "workspace",
    latest_sequence: 0,
    pending_approvals: 0,
    pending_inputs: 0,
    checkpoint_count: 0,
    changed_files: 0,
    total_tokens: 0,
    cost_microunits: 0,
    cost_known: true,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z"
  };
  return {
    phase: "ready",
    workspaceRoot: "/workspace",
    includeArchived: false,
    contextResources: [],
    messageFeedback: {},
    sessions: [session],
    sessionSearchQuery: "",
    sessionSearchResults: [],
    selectedSessionID: session.session_id,
    hydratingSessionID: "",
    events,
    queuedTurns: [],
    conversation: projectConversation(events),
    historyMoreBefore: false,
    providers: [
      {
        id: "fixture",
        display_name: "Fixture",
        selected: true,
        availability: "available"
      },
      {
        id: "offline",
        display_name: "Offline",
        selected: false,
        availability: "unavailable",
        reason: "Credential missing"
      }
    ],
    models: [
      {
        provider: "fixture",
        id: "fixture",
        selected: true,
        capabilities: modelCapabilities("Fixture")
      },
      {
        provider: "fixture",
        id: "reasoner",
        selected: false,
        capabilities: {
          ...modelCapabilities("Reasoner"),
          reasoning: true,
          reasoning_efforts: ["low", "high"]
        }
      }
    ],
		workspaces: [{
			id: "workspace-id",
			root: "/workspace",
			label: "workspace",
			ready: true,
			removable: true,
			session_count: 1
		}],
		selectedWorkspaceID: "workspace-id",
    profile: {
      profile: {
        version: 1,
        revision: 1,
        mode: "act",
        planning_policy: "adaptive",
        provider: "fixture",
        model: "fixture",
        approval_posture: "suggest",
        execution_target: "local",
        max_steps: 32,
        enabled_tool_ids: ["read"]
      },
      capabilities: {
        provider: "fixture",
        model: "fixture",
        mutable_fields: [
          "mode",
          "provider", "model", "reasoning_effort",
          "approval_posture", "execution_target", "max_steps",
          "enabled_tool_ids"
        ],
        model_capabilities: modelCapabilities("Fixture")
      }
    },
    tools: [{
      id: "read",
      name: "read_file",
      description: "Read a file",
      source_kind: "builtin",
      source_label: "QCode",
      capability: "read",
      access_mode: "read",
      risk_level: "read",
      sandbox_requirement: "strong",
      policy_state: "deferred",
      policy_reason: "Validated when invoked",
      constitution_state: "deferred",
      constitution_reason: "Validated when invoked",
      availability: "available",
      state: "active",
      revision: 1,
      enabled: true,
      guarded: true
    }],
    checkpoints: [],
    agents: [],
    extensions: [],
    tracePhase: "idle",
    socketConnected: true
  };
}

async function openContextDetails(): Promise<void> {
  fireEvent.click(screen.getByRole("button", {name: "Commands"}));
  fireEvent.click(screen.getByRole("menuitem", {name: /context/}));
  await screen.findByRole("dialog", {name: "Add context"});
}

function mockClient(value: RuntimeSnapshot): RuntimeClient {
  return {
    withdrawTurn: vi.fn(async () => {}),
    subscribe: () => () => {},
    getSnapshot: () => value,
    start: vi.fn(async () => {}),
    stop: vi.fn(),
    loadEarlierHistory: vi.fn(async () => 0),
    refreshSessions: vi.fn(async () => {}),
		refreshWorkspaces: vi.fn(async () => ({
			version: 1,
			workspaces: value.workspaces
		})),
		addWorkspace: vi.fn(async () => {}),
		removeWorkspace: vi.fn(async () => {}),
		pickWorkspaceDirectory: vi.fn(async () => ({
			path: "/workspace/secondary"
		})),
		selectWorkspace: vi.fn(async () => {}),
    switchWorkspaceBranch: vi.fn(async () => {}),
    gitOverview: vi.fn(async (workspaceID: string) => ({
      repository: false,
      ...value.workspaces.find((workspace) => workspace.id === workspaceID)?.git,
      files: [],
      remotes: []
    })),
    setArchivedVisible: vi.fn(async () => {}),
    createSession: vi.fn(async () => {}),
		completeSetup: vi.fn(async () => {}),
    probeSetup: vi.fn(async () => ({
      models: [{
        id: "vendor-model",
        context_tokens: 200000,
        max_output_tokens: 24000
      }],
      capabilities: {
        streaming: true,
        reasoning: true,
        tool_calls: true,
        native_search: false,
        incremental_responses: false,
        vision: false,
        image_input: false,
        prompt_cache: false,
        automatic_prompt_cache: false,
        thinking_toggle: false
      }
    })),
    selectSession: vi.fn(async () => {}),
    updateSession: vi.fn(async () => {}),
    deleteSession: vi.fn(async () => {}),
    loadDraft: vi.fn(() => ""),
    saveDraft: vi.fn(),
    submitPrompt: vi.fn(async () => ({})),
    steer: vi.fn(async () => ({})),
    enqueue: vi.fn(async () => ({})),
    updateQueuedTurn: vi.fn(async () => ({})),
    removeQueuedTurn: vi.fn(async () => ({})),
    promoteQueuedTurn: vi.fn(async () => ({})),
    cancel: vi.fn(async () => ({})),
    decideApproval: vi.fn(async () => ({})),
    replyInput: vi.fn(async () => ({})),
    recoverTurn: vi.fn(async () => ({})),
    updateProfile: vi.fn(async (patch: Record<string, unknown>) => ({
      profile: {
        ...value.profile!.profile,
        ...patch,
        revision: value.profile!.profile.revision + 1
      },
      prompt_cache_reset: Boolean(
        patch.model || patch.provider || patch.reasoning_effort ||
        patch.enabled_tool_ids || patch.mode
      ),
      reset_reason: Object.keys(patch).join(",")
    })),
    listAgentPresets: vi.fn(async () => ({
      version: 1,
      revision: 0,
      presets: []
    })),
    saveAgentPreset: vi.fn(async () => ({
      version: 1,
      revision: 1
    })),
    deleteAgentPreset: vi.fn(async () => ({
      version: 1,
      revision: 1
    })),
    applyAgentPreset: vi.fn(async () => ({
      version: 1,
      preset_id: "preset",
      profile_update: {
        profile: value.profile!.profile,
        prompt_cache_reset: false
      },
      restart_required: false
    })),
    restoreCheckpoint: vi.fn(async () => ({})),
    forkCheckpoint: vi.fn(async () => ({})),
    setExtensionEnabled: vi.fn(async () => ({})),
    controlExtension: vi.fn(async () => ({
      revision: 1,
      detail: {status: "ready"}
    })),
    credentialStatus: vi.fn(async () => ({
      reference: {kind: "none", name: ""},
      configured: false,
      validation: "not_validated"
    })),
    connectionStatus: vi.fn(async () => ({
      provider: "fixture",
      endpoint: "https://models.example.com/v1",
      protocol: "openai_chat"
    })),
    diagnostics: vi.fn(async () => ({
      ready: true,
      draining: false,
      runtime_health: {},
      mcp_health: []
    })),
    setKeyringCredential: vi.fn(async () => ({})),
    validateCredential: vi.fn(async () => ({})),
    testModel: vi.fn(async (model: string) => ({
      provider: "fixture",
      model,
      status: "available" as const,
      detail: "Connection succeeded and the provider listed this model",
      tested_at: "2026-01-01T00:00:00Z"
    })),
    probeModel: vi.fn(async () => ({
      models: [{
        id: "model-released-today",
        context_tokens: 200000,
        max_output_tokens: 24000
      }],
      capabilities: {
        streaming: true,
        reasoning: true,
        reasoning_efforts: ["low", "medium", "high"],
        default_reasoning_effort: "medium",
        tool_calls: true,
        native_search: false,
        incremental_responses: false,
        vision: false,
        image_input: false,
        prompt_cache: false,
        automatic_prompt_cache: false,
        thinking_toggle: false
      }
    })),
    addModel: vi.fn(async () => ({version: 1, models: []})),
    updateModel: vi.fn(async () => ({version: 1, models: []})),
    removeModel: vi.fn(async () => ({version: 1, models: []})),
    clearKeyringCredential: vi.fn(async () => ({})),
    browseWorkspace: vi.fn(async () => ({
      path: ".",
      entries: [],
      more: false
    })),
    readWorkspaceResource: vi.fn(async () => ({
      path: "README.md",
      uri: "file:///workspace/README.md",
      document_version: 1,
      content: "",
      digest: "0".repeat(64),
      bytes: 0,
      content_handle: "content"
    })),
    readWorkspaceImage: vi.fn(async () => {
      throw new Error("image not configured");
    }),
    downloadWorkspaceContent: vi.fn(async () => new Blob()),
    workspaceDiff: vi.fn(async () => ({
      session_id: "session",
      thread_id: "thread",
      diff: "",
      digest: "0".repeat(64)
    })),
    searchWorkspaceSymbols: vi.fn(async () => ({
      query: "",
      status: "ready",
      symbols: []
    })),
    workspaceDiagnostics: vi.fn(async () => ({
      session_id: "session",
      thread_id: "thread",
      diagnostics: []
    })),
    addGitDiffContext: vi.fn(),
    addTerminalContext: vi.fn(async () => {}),
    addSymbolContext: vi.fn(),
    addDiagnosticsContext: vi.fn(),
    addImageContext: vi.fn(),
    addAttachmentContext: vi.fn(),
    removeAttachmentContext: vi.fn(),
    refreshTrace: vi.fn(async () => {}),
    compactThread: vi.fn(async () => ({})),
    toggleMessageFeedback: vi.fn()
  } as unknown as RuntimeClient;
}

function modelCapabilities(displayName: string) {
  return {
    display_name: displayName,
    context_window: 128_000,
    max_output_tokens: 8_192,
    streaming: true,
    reasoning: false,
    tool_calls: true,
    parallel_tool_calls: "unknown" as const,
    native_search: false,
    incremental_responses: false,
    vision: false,
    image_input: false,
    prompt_cache: true,
    automatic_prompt_cache: false,
    thinking_toggle: false,
    metadata_provenance: {
      canonical_id: "fixture",
      wire_id: "fixture",
      limits: "fixture",
      capabilities: "fixture",
      pricing: "fixture"
    },
    credential_status: "configured" as const,
    availability: "available" as const,
    selection_mode: "hot" as const
  };
}

function fixtureFile(name: string, type: string, content: string): File {
  return fixtureBytes(name, type, new TextEncoder().encode(content));
}

function fixtureBytes(name: string, type: string, bytes: Uint8Array): File {
  return {
    name,
    type,
    size: bytes.byteLength,
    arrayBuffer: vi.fn(async () => Uint8Array.from(bytes).buffer)
  } as unknown as File;
}

function event(
  sequence: number,
  kind: string,
  data: Record<string, unknown>
): RuntimeEvent {
  return {
    version: 1,
    id: `event-${sequence}`,
    kind,
    operation_id: "operation",
    thread_id: "thread",
    turn_id: "turn",
    item_id: `item-${sequence}`,
    sequence,
    created_at: "2026-01-01T00:00:00Z",
    data
  };
}
