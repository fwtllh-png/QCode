import {describe, expect, it} from "vitest";
import type {RuntimeEvent} from "../protocol";
import {
  ConversationProjection,
  projectConversation
} from "./conversation";

describe("ConversationProjection", () => {
  it.each(["completed", "failed", "canceled", "timed_out"])(
    "replays a yielded command through its original call to %s",
    (status) => {
      const events = [
        event(1, "tool.start", {
          call_id: "start", tool: "exec_command", arguments: {command: "test command"}
        }),
        event(2, "tool.result", {call_id: "start", tool: "exec_command", output: "", is_error: false}),
        event(3, "command.execution", {
          call_id: "start", session_id: "term-1", command: "test command", status: "started"
        })
      ];
      const initial = projectConversation(events);
      expect(initial.nodes.get("tool-start")).toMatchObject({
        state: "running", command: {status: "started"}
      });
      events.push(
        event(4, "tool.start", {call_id: "poll", tool: "write_stdin", arguments: {session_id: "term-1"}}),
        event(5, "tool.result", {call_id: "poll", tool: "write_stdin", output: "done", is_error: status !== "completed"}),
        event(6, "command.execution", {
          call_id: "start", session_id: "term-1", command: "test command",
          status, exit_code: status === "completed" ? 0 : 1
        })
      );
      const replay = projectConversation(events);
      expect(replay.nodes.get("tool-start")).toMatchObject({
        state: status === "completed" ? "completed" : "failed",
        command: {status}
      });
      expect(replay.nodes.get("tool-poll")).not.toHaveProperty("command");
    }
  );

  it("coalesces repeated in-turn compaction updates", () => {
    const snapshot = projectConversation([
      event(1, "turn.compaction", {
        phase: "mid_turn",
        summary: "Pruned tool results"
      }),
      event(2, "turn.compaction", {
        phase: "mid_turn",
        summary: "Retained deterministic truth"
      })
    ]);
    const context = snapshot.order
      .map((id) => snapshot.nodes.get(id))
      .filter((node) => node?.kind === "context");

    expect(context).toHaveLength(1);
    expect(context[0]).toMatchObject({
      id: "context-turn",
      sequence: 2,
      summary: "Retained deterministic truth"
    });
  });

  it("hides post-turn narrative maintenance from the transcript", () => {
    const snapshot = projectConversation([
      event(1, "turn.compaction", {
        phase: "post_turn",
        status: "fallback",
        mode: "post_turn",
        summary: "semantic narrative unavailable; retained deterministic truth and raw tail",
        fallback_reason: "narrative provider output is incomplete"
      }),
      event(2, "turn.compaction", {
        phase: "post_turn",
        status: "completed",
        mode: "post_turn",
        summary: "semantic narrative committed for the compacted context",
        narrative_included: true
      })
    ]);

    expect(snapshot.order
      .map((id) => snapshot.nodes.get(id))
      .filter((node) => node?.kind === "context")).toEqual([]);
  });

  it("still shows in-turn history replacement", () => {
    const snapshot = projectConversation([
      event(1, "turn.compaction", {
        phase: "mid_turn",
        status: "completed",
        summary: "compacted history",
        removed_messages: 8,
        original_bytes: 32000,
        retained_bytes: 12000
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "context",
      title: "Compaction",
      summary: "compacted history"
    });
  });

  it("replaces intermediate verification with the final verdict", () => {
    const snapshot = projectConversation([
      event(1, "turn.verification", {status: "unavailable", action: "repair"}),
      event(2, "turn.verification", {status: "passed", action: "passed"})
    ]);
    const verification = snapshot.order
      .map((id) => snapshot.nodes.get(id))
      .filter((node) => node?.kind === "status");

    expect(verification).toMatchObject([
      {
        title: "Checks passed",
        text: "Changed files are covered by recorded checks.",
        failed: false
      }
    ]);
  });

  it("explains an unavailable verification without internal terminology", () => {
    const snapshot = projectConversation([
      event(1, "turn.verification", {
        status: "unavailable",
        message: "structured quality evidence does not cover every changed path"
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      title: "Not fully verified",
      text: "No structured check covered every changed file.",
      failed: false
    });
  });

  it("derives recovery actions and side effects from Runtime facts", () => {
    const snapshot = projectConversation([
      event(1, "turn.failed", {
        code: "unavailable",
        message: "provider unavailable",
        fault: {
          disposition: "resume_turn",
          side_effects: "rolled_back",
          recovery_action: "continue from durable state"
        }
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "status",
      recovery: {
        canRetry: false,
        canContinue: true,
        sideEffects: "rolled_back",
        action: "continue from durable state"
      }
    });
  });

  it("presents token admission failures without protocol jargon", () => {
    const snapshot = projectConversation([
      event(1, "turn.failed", {
        code: "resource_exhausted",
        message: "token budget exhausted: projected 1139573, limit 1048576",
        fault: {
          disposition: "resume_turn",
          side_effects: "draft"
        }
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "status",
      title: "Token limit reached",
      text: "The next model call would exceed this run's token limit (1,139,573 projected, 1,048,576 allowed)."
    });
  });

  it("presents declared incomplete turns as resumable blocked work", () => {
    const snapshot = projectConversation([
      event(1, "turn.failed", {
        code: "conflict",
        message: "turn declared incomplete with resumable pending actions",
        convergence: {
          cause: "declared_incomplete",
          used: 0,
          limit: 0,
          summary: "The environment is missing a required dependency.",
          pending_actions: ["Install the dependency and continue."]
        }
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "status",
      title: "Blocked",
      text: "The environment is missing a required dependency.",
      failed: false,
      blocked: true,
      recoverable: true
    });
  });

  it("presents recoverable Runtime faults as blocked work", () => {
    const snapshot = projectConversation([
      event(1, "turn.failed", {
        code: "unavailable",
        message: "workspace journal has a retained draft",
        fault: {
          disposition: "resume_turn",
          side_effects: "draft",
          recovery_action: "continue the retained draft"
        }
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "status",
      title: "Blocked",
      failed: false,
      blocked: true,
      recoverable: true
    });
  });

  it("offers recovery for current and legacy Provider retry faults", () => {
    for (const disposition of ["retry_step", "retry_turn"]) {
      const snapshot = projectConversation([
        event(1, "turn.failed", {
          code: "unavailable",
          message: "provider rate limit retry budget exhausted",
          fault: {
            disposition,
            side_effects: "draft",
            recovery_action: "retry the turn from its durable checkpoint"
          }
        })
      ]);

      expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
        kind: "status",
        title: "Blocked",
        blocked: true,
        recoverable: true,
        recovery: {
          canRetry: true,
          canContinue: true,
          sideEffects: "draft"
        }
      });
    }
  });

  it("distinguishes rate-limit wait, incomplete continuation, and tool boundaries", () => {
    const snapshot = projectConversation([
      turnEvent(1, "turn-one", "provider.attempt", {
        sample_id: "sample-1",
        attempt: 1,
        status: "retry_wait",
        failure_code: "rate_limit",
        http_status: 429
      }),
      turnEvent(2, "turn-one", "provider.attempt", {
        sample_id: "sample-1",
        attempt: 2,
        status: "incomplete",
        stop_reason: "max_tokens"
      }),
      turnEvent(3, "turn-one", "provider.attempt", {
        sample_id: "sample-1",
        attempt: 3,
        status: "completed",
        stop_reason: "tool_use"
      }),
      turnEvent(4, "turn-one", "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      turnEvent(5, "turn-one", "tool.result", {
        call_id: "call-1",
        tool: "file_read",
        output: "# Project"
      })
    ]);
    expect(snapshot.nodes.has("provider-state-turn-one")).toBe(false);
  });

  it("does not treat model cut-off prose as a runtime state", () => {
    const snapshot = projectConversation([
      turnEvent(1, "turn-one", "reasoning.completed", {
        text: "Your message was cut off after the tool call."
      }),
      turnEvent(2, "turn-one", "provider.attempt", {
        sample_id: "sample-1",
        attempt: 1,
        status: "completed",
        stop_reason: "tool_use"
      })
    ]);
    const status = [...snapshot.nodes.values()].filter((node) => node.kind === "status");
    expect(status).toEqual([]);
  });

  it("projects normal tool boundaries only into the running status", () => {
    const projection = new ConversationProjection();
    projection.apply(turnEvent(1, "turn-one", "turn.started", {prompt: "Inspect"}));
    projection.apply(turnEvent(2, "turn-one", "provider.attempt", {
      status: "completed", stop_reason: "tool_use"
    }));
    expect(projection.snapshot().activeStatus).toBe("Preparing tools...");
    projection.apply(turnEvent(3, "turn-one", "tool.start", {call_id: "a", tool: "file_read"}));
    projection.apply(turnEvent(4, "turn-one", "tool.start", {call_id: "b", tool: "file_read"}));
    expect(projection.snapshot().activeStatus).toBe("Running tools...");
    projection.apply(turnEvent(5, "turn-one", "tool.result", {call_id: "a", tool: "file_read"}));
    expect(projection.snapshot().activeStatus).toBe("Running tools...");
    projection.apply(turnEvent(6, "turn-one", "tool.result", {call_id: "b", tool: "file_read"}));
    const continued = projection.snapshot();
    expect(continued.activeStatus).toBe("Continuing...");
    expect([...continued.nodes.values()].some((node) => node.kind === "status")).toBe(false);
    projection.apply(turnEvent(7, "turn-one", "provider.attempt", {
      status: "retry_wait", failure_code: "rate_limit"
    }));
    expect(projection.snapshot().nodes.get("provider-state-turn-one")).toMatchObject({
      warning: true, title: "Provider rate limited"
    });
    projection.apply(turnEvent(8, "turn-one", "turn.completed", {text: "Done"}));
    expect(projection.snapshot().activeStatus).toBeUndefined();
    expect(projection.snapshot().nodes.has("provider-state-turn-one")).toBe(false);
  });

  it("presents a user interruption as paused rather than failed", () => {
    const snapshot = projectConversation([
      event(1, "turn.canceled", {reason: "user_interrupted"})
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "status",
      title: "Paused",
      text: "Paused by user.",
      failed: false,
      warning: true,
      recoverable: true
    });
  });

  it("restores running status after approvals and input resolve", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {prompt: "Inspect"}));
    projection.apply(event(2, "tool.start", {call_id: "a", tool: "shell"}));
    projection.apply(event(3, "approval.required", {request_id: "one", call_id: "a"}));
    projection.apply(event(4, "approval.required", {request_id: "two", call_id: "a"}));
    projection.apply(event(5, "approval.resolved", {request_id: "one", decision: "allow"}));
    expect(projection.snapshot().activeStatus).toBe("Awaiting approval...");
    projection.apply(event(6, "approval.resolved", {request_id: "two", decision: "allow"}));
    expect(projection.snapshot().activeStatus).toBe("Running tools...");
    projection.apply(event(7, "input.required", {request_id: "input"}));
    projection.apply(event(8, "tool.result", {call_id: "a"}));
    expect(projection.snapshot().activeStatus).toBe("Waiting for input...");
    projection.apply(event(9, "input.resolved", {request_id: "input"}));
    expect(projection.snapshot().activeStatus).toBe("Continuing...");
  });

  it("projects produced files and marks older paths stale", () => {
    const first = [
      turnEvent(1, "turn-one", "turn.started", {display_prompt: "Edit"}),
      turnEvent(2, "turn-one", "tool.start", {
        call_id: "edit-one",
        tool: "file_edit",
        arguments: {path: "main.go", old: "old\n", new: "new\n"}
      }),
      turnEvent(3, "turn-one", "tool.result", {
        call_id: "edit-one",
        tool: "file_edit",
        output: "updated",
        changes: [{path: "main.go", kind: "modified", added: 1, removed: 1}]
      }),
      turnEvent(4, "turn-one", "turn.receipt", {
        changes: [{
          path: "main.go",
          tool: "file_edit",
          kind: "modified",
          added: 1,
          removed: 1
        }],
        verification: {tests: "passed", diagnostics: "not_evaluated"}
      }),
      turnEvent(5, "turn-one", "turn.completed", {text: "First"})
    ];
    const snapshot = projectConversation([
      ...first,
      turnEvent(6, "turn-two", "turn.started", {display_prompt: "Edit again"}),
      turnEvent(7, "turn-two", "turn.receipt", {
        changes: [{
          path: "main.go",
          tool: "file_write",
          kind: "modified",
          added: 2,
          removed: 1
        }],
        verification: {tests: "not_evaluated"}
      }),
      turnEvent(8, "turn-two", "turn.completed", {text: "Second"})
    ]);
    const deliverables = snapshot.order.flatMap((id) => {
      const node = snapshot.nodes.get(id);
      return node?.kind === "deliverables" ? [node] : [];
    });

    expect(deliverables).toMatchObject([
      {
        turnID: "turn-one",
        verification: "passed",
        files: [{
          path: "main.go",
          callID: "edit-one",
          stale: true,
          diff: {before: "old\n", after: "new\n"}
        }]
      },
      {
        turnID: "turn-two",
        verification: "unverified",
        files: [{path: "main.go", stale: false}]
      }
    ]);
  });

  it("matches a full rebuild after incremental application", () => {
    const events = [
      event(1, "turn.started", {display_prompt: "Inspect the workspace"}),
      event(2, "reasoning.delta", {sample_id: "sample-1", text: "Find files\n"}),
      event(3, "reasoning.delta", {sample_id: "sample-1", text: "Read the manifest"}),
      event(4, "reasoning.completed", {
        sample_id: "sample-1",
        text: "Find files\nRead the manifest"
      }),
      event(5, "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(6, "tool.output", {call_id: "call-1", chunk: "# Code"}),
      event(7, "tool.result", {
        call_id: "call-1",
        output: "# QCode",
        is_error: false
      }),
      event(8, "output.delta", {text: "Done"}),
      event(9, "turn.receipt", {
        outcome: "answered",
        input_tokens: 20,
        output_tokens: 4
      }),
      event(10, "turn.completed", {text: "Done", outcome: "answered"})
    ];
    const incremental = new ConversationProjection();
    for (const value of events) incremental.apply(value);

    expect(serializable(incremental.snapshot())).toEqual(
      serializable(projectConversation(events))
    );
  });

  it("replaces transient reasoning with one durable block per model sample", () => {
    const snapshot = projectConversation([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "reasoning.delta", {sample_id: "one", text: "First draft"}),
      event(3, "reasoning.completed", {sample_id: "one", text: "First thought"}),
      event(4, "tool.start", {
        call_id: "call",
        tool: "file_read",
        arguments: {path: "README.md"}
      }),
      event(5, "tool.result", {
        call_id: "call",
        output: "content",
        is_error: false
      }),
      event(6, "reasoning.completed", {sample_id: "two", text: "Second thought"}),
      event(7, "turn.completed", {text: "Done"})
    ]);
    const reasoning = snapshot.order.flatMap((id) => {
      const node = snapshot.nodes.get(id);
      return node?.kind === "reasoning" ? [node] : [];
    });

    expect(reasoning).toMatchObject([
      {text: "First thought", running: false},
      {text: "Second thought", running: false}
    ]);
  });

  it("keeps settled node references stable when a different node streams", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Question"}));
    projection.apply(event(2, "tool.start", {
      call_id: "call",
      tool: "file_read",
      arguments: {path: "README.md"}
    }));
    projection.apply(event(3, "tool.result", {
      call_id: "call",
      output: "result",
      is_error: false
    }));
    const settled = projection.snapshot().nodes.get("tool-call");

    projection.apply(event(4, "output.delta", {text: "A"}));
    projection.apply(event(5, "output.delta", {text: "B"}));

    expect(projection.snapshot().nodes.get("tool-call")).toBe(settled);
  });

  it("streams draft text live and settles it with the completed turn", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Summarize"}));
    projection.apply(event(2, "output.draft", {text: "The answer ", sample_id: "s1"}));
    projection.apply(event(3, "output.draft", {text: "is done.", sample_id: "s1"}));

    const streaming = projection.snapshot().nodes.get("output-turn");
    expect(streaming).toMatchObject({kind: "assistant", text: "The answer is done."});

    projection.apply(event(4, "turn.completed", {text: "The answer is done."}));
    expect(projection.snapshot().nodes.get("output-turn")).toMatchObject({
      kind: "assistant",
      text: "The answer is done."
    });
  });

  it("retracts a discarded sample's draft without touching other samples", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Inspect"}));
    projection.apply(event(2, "output.draft", {text: "I will check.", sample_id: "s1"}));
    projection.apply(event(3, "output.discarded", {sample_id: "s1", reason: "narration"}));
    projection.apply(event(4, "tool.start", {
      call_id: "call",
      tool: "file_read",
      arguments: {path: "main.go"}
    }));
    projection.apply(event(5, "tool.result", {call_id: "call", output: "ok", is_error: false}));
    projection.apply(event(6, "output.draft", {text: "It is fine.", sample_id: "s2"}));

    expect(projection.snapshot().nodes.get("output-turn")).toMatchObject({
      kind: "assistant",
      text: "It is fine."
    });
  });

  it("renders the streamed answer below the turn's tool work it follows", () => {
    // The narration draft creates the assistant node before the tools run;
    // after retraction and the final sample, the surviving answer must render
    // below the tool activity, not above it.
    const snapshot = projectConversation([
      event(1, "turn.started", {display_prompt: "inspect"}),
      event(2, "output.draft", {text: "I will check.", sample_id: "s1"}),
      event(3, "output.discarded", {sample_id: "s1", reason: "narration"}),
      event(4, "tool.start", {
        call_id: "call",
        tool: "file_read",
        arguments: {path: "a.go"}
      }),
      event(5, "tool.result", {call_id: "call", output: "ok", is_error: false}),
      event(6, "output.draft", {text: "The file is fine.", sample_id: "s2"}),
      event(7, "turn.completed", {text: "The file is fine."})
    ]);
    const kinds = snapshot.order.flatMap((id) => {
      const node = snapshot.nodes.get(id);
      return node ? [node.kind] : [];
    });
    expect(kinds).toEqual(["user", "tool", "assistant"]);
    const answer = snapshot.nodes.get("output-turn");
    expect(answer).toMatchObject({kind: "assistant", text: "The file is fine."});
  });

  it("keeps replayed answers after tool work and live answers at the stream tail", () => {
    // Replayed turns (durable output.delta only) already order correctly; a
    // second live turn with narration before its tools must not disturb them.
    const snapshot = projectConversation([
      event(1, "turn.started", {display_prompt: "first"}),
      event(2, "tool.start", {
        call_id: "call-1",
        tool: "file_read",
        arguments: {path: "a.go"}
      }),
      event(3, "tool.result", {call_id: "call-1", output: "ok", is_error: false}),
      event(4, "output.delta", {text: "first answer"}),
      event(5, "turn.completed", {text: "first answer"}),
      turnEvent(6, "turn2", "turn.started", {display_prompt: "second"}),
      turnEvent(7, "turn2", "output.draft", {text: "Let me look.", sample_id: "n1"}),
      turnEvent(8, "turn2", "output.discarded", {sample_id: "n1", reason: "narration"}),
      turnEvent(9, "turn2", "tool.start", {
        call_id: "call-2",
        tool: "file_read",
        arguments: {path: "b.go"}
      }),
      turnEvent(10, "turn2", "tool.result", {call_id: "call-2", output: "ok", is_error: false}),
      turnEvent(11, "turn2", "output.draft", {text: "second answer", sample_id: "s1"}),
      turnEvent(12, "turn2", "turn.completed", {text: "second answer"})
    ]);
    expect(snapshot.order).toEqual([
      "event-1",
      "tool-call-1",
      "output-turn",
      "turn2-event-6",
      "tool-call-2",
      "output-turn2"
    ]);
  });

  it("tracks the newest main-thread turn past settlement for plan recency", () => {
    const snapshot = projectConversation([
      event(1, "turn.started", {display_prompt: "first"}),
      event(2, "turn.completed", {text: "first answer"}),
      turnEvent(3, "turn2", "turn.started", {display_prompt: "second"}),
      turnEvent(4, "turn2", "turn.completed", {text: "second answer"}),
      // Child-agent turns replay on their own thread and must not advance the
      // main thread's latest turn.
      event(5, "agent.spawned", {agent_id: "child", detail: {thread_id: "child-thread"}}),
      {
        ...turnEvent(6, "child-turn", "turn.started", {display_prompt: "child work"}),
        thread_id: "child-thread"
      },
      {
        ...turnEvent(7, "child-turn", "turn.completed", {text: "child answer"}),
        thread_id: "child-thread"
      }
    ]);

    expect(snapshot.activeTurnID).toBe("");
    expect(snapshot.latestTurnID).toBe("turn2");
  });

  it("ignores retraction for an unknown sample or turn", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Answer"}));
    projection.apply(event(2, "output.draft", {text: "Kept.", sample_id: "s1"}));
    projection.apply(event(3, "output.discarded", {sample_id: "missing", reason: "narration"}));

    expect(projection.snapshot().nodes.get("output-turn")).toMatchObject({
      kind: "assistant",
      text: "Kept."
    });
  });

  it("summarizes result handles without exposing output by default", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "result",
        tool: "result_get",
        arguments: {handle: "sha256:abc"}
      }),
      event(2, "tool.result", {
        call_id: "result",
        output: "long result",
        is_error: false
      })
    ]);
    const node = snapshot.nodes.get("tool-result");

    expect(node).toMatchObject({
      kind: "tool",
      summary: "Read full result · sha256:abc",
      state: "completed"
    });
  });

  it("keeps expected plan-gate retries out of chat", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "spawn-before-plan",
        tool: "spawn_agent",
        arguments: {task_name: "audit"}
      }),
      event(2, "tool.result", {
        call_id: "spawn-before-plan",
        tool: "spawn_agent",
        output: "localized policy message",
        is_error: true,
        recovery: {
          error_category: "plan_required",
          required_action: "submit_plan",
          retry_original: false
        }
      })
    ]);

    expect(snapshot.nodes.has("tool-spawn-before-plan")).toBe(false);
    expect(snapshot.order).not.toContain("tool-spawn-before-plan");
  });

  it("tracks pending interaction and active turn state from facts", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Change it"}));
    projection.apply(event(2, "approval.required", {request_id: "approval"}));
    expect(projection.snapshot()).toMatchObject({
      activeTurnID: "turn",
      pendingApproval: {id: "event-2"}
    });

    projection.apply(event(3, "approval.resolved", {request_id: "approval"}));
    projection.apply(event(4, "turn.completed", {text: "Changed"}));
    expect(projection.snapshot()).toMatchObject({
      activeTurnID: "",
      pendingApproval: undefined
    });
  });

  it("finishes an active turn when its start operation is rejected", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Push it"}));
    projection.apply(event(2, "reasoning.delta", {text: "Working"}));
    projection.apply(event(3, "approval.required", {request_id: "approval"}));
    projection.apply(event(4, "operation.rejected", {
      code: "unavailable",
      message: "terminal envelope could not be committed"
    }));

    expect(projection.snapshot()).toMatchObject({
      activeTurnID: "",
      pendingApproval: undefined
    });
    const reasoning = [...projection.snapshot().nodes.values()].find(
      (node) => node.kind === "reasoning"
    );
    expect(reasoning).toMatchObject({
      kind: "reasoning",
      running: false
    });
  });

  it("keeps the turn active when a child operation is rejected", () => {
    const projection = new ConversationProjection();
    projection.apply(event(1, "turn.started", {display_prompt: "Push it"}));
    projection.apply({
      ...event(2, "operation.rejected", {
        code: "conflict",
        message: "approval decision rejected"
      }),
      operation_id: "child-operation"
    });

    expect(projection.snapshot().activeTurnID).toBe("turn");
  });

  it("projects accepted steering as a durable user interjection", () => {
    const snapshot = projectConversation([
      event(1, "turn.started", {display_prompt: "Inspect"}),
      event(2, "output.delta", {text: "Initial direction"}),
      event(3, "turn.steered", {prompt: "Focus on the parser"}),
      event(4, "output.delta", {text: "Revised direction"}),
      event(5, "turn.completed", {text: "Done"})
    ]);

    expect(snapshot.order.map((id) => snapshot.nodes.get(id))).toMatchObject([
      {kind: "user", text: "Inspect", images: []},
      {kind: "assistant", text: "Initial direction", superseded: true},
      {kind: "user", text: "Focus on the parser", images: [], steering: true},
      {kind: "assistant", text: "Done"}
    ]);
  });

  it("projects durable image inputs onto the user message", () => {
    const snapshot = projectConversation([
      event(1, "turn.started", {
        display_prompt: "Describe this image",
        images: [{
          label: "lake.png",
          media_type: "image/png",
          content: "aW1hZ2U="
        }]
      })
    ]);

    expect(snapshot.nodes.get(snapshot.order[0])).toMatchObject({
      kind: "user",
      text: "Describe this image",
      images: [{
        label: "lake.png",
        mediaType: "image/png",
        content: "aW1hZ2U="
      }]
    });
  });

  it("retains an approval edit plan on the associated tool result", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "write-1",
        tool: "file_edit",
        arguments: {path: "main.go", old: "old", new: "new"}
      }),
      event(2, "approval.required", {
        request_id: "approval-1",
        call_id: "write-1",
        tool: "file_edit",
        edit_plan: {
          id: "plan-1",
          diff: "--- a/main.go\n+++ b/main.go\n-old\n+new\n",
          files: [{
            path: "main.go",
            kind: "modified",
            before: "old\n",
            after: "new\n",
            before_exists: true,
            after_exists: true
          }]
        }
      }),
      event(3, "approval.resolved", {
        request_id: "approval-1",
        decision: "approve"
      }),
      event(4, "tool.result", {
        call_id: "write-1",
        tool: "file_edit",
        output: "modified main.go +1 -1",
        changes: [{path: "main.go", kind: "modified", added: 1, removed: 1}],
        is_error: false
      })
    ]);

    expect(snapshot.nodes.get("tool-write-1")).toMatchObject({
      kind: "tool",
      variant: "diff",
      summary: "main.go · +1 -1",
      approvalDecision: "approve",
      editPlan: {
        id: "plan-1",
        files: [{
          path: "main.go",
          before: "old\n",
          after: "new\n"
        }]
      }
    });
  });

  it("keeps shell output visible when a command changes generated files", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "compile-1",
        tool: "exec_command",
        arguments: {command: "c++ source.cpp -o build/test"}
      }),
      event(2, "tool.result", {
        call_id: "compile-1",
        tool: "exec_command",
        output: "compile_exit=0\ntest_exit=1",
        changes: [{
          path: "build/test",
          kind: "modified",
          added: 0,
          removed: 0
        }],
        is_error: true
      }),
      event(3, "command.execution", {
        call_id: "compile-1",
        command: "c++ source.cpp -o build/test && ./build/test",
        status: "failed",
        exit_code: 1
      })
    ]);

    expect(snapshot.nodes.get("tool-compile-1")).toMatchObject({
      kind: "tool",
      variant: "shell",
      state: "failed",
      summary: "c++ source.cpp -o build/test && ./build/test",
      errorSummary: 'Command exited with code 1 · output: "test_exit=1"',
      output: "compile_exit=0\ntest_exit=1",
      changes: [{path: "build/test"}],
      command: {exitCode: 1}
    });
  });

  it.each(["result-first", "command-first"])(
    "keeps normal output separate from exit failure with %s delivery and replay",
    (order) => {
      const command = "sed -n '1,20p' service.go; ls proto 2>/dev/null";
      const result = {
        call_id: "probe",
        output: "package service\n\nfunc Serve() {}",
        is_error: true,
        execution: {
          terminal_status: "failed",
          attempts: [{status: "failed", reason: "tool_error"}]
        }
      };
      const execution = {call_id: "probe", command, status: "failed", exit_code: 1};
      const events = [
        event(1, "tool.start", {
          call_id: "probe", tool: "exec_command", arguments: {command}
        }),
        ...(order === "result-first"
          ? [event(2, "tool.result", result), event(3, "command.execution", execution)]
          : [event(2, "command.execution", execution), event(3, "tool.result", result)])
      ];
      const projection = new ConversationProjection();
      for (const item of events) {
        projection.apply(item);
        projection.snapshot();
      }
      const node = projection.snapshot().nodes.get("tool-probe");
      expect(node).toMatchObject({
        summary: command,
        state: "failed",
        output: result.output,
        errorSummary: 'Command exited with code 1 · output: "func Serve() {}"',
        command: {exitCode: 1}
      });
      expect(projectConversation(events).nodes.get("tool-probe")).toEqual(node);
    }
  );

  it.each([
    {
      recovery: {error_category: "file_not_found", path: "proto", required_action: "file_list"},
      execution: undefined,
      expected: "file not found · proto · Required action: file_list"
    },
    {
      recovery: undefined,
      execution: {
        terminal_status: "failed",
        attempts: [
          {status: "failed", denial: {reason_code: "network_denied", resource: "old.example"}},
          {status: "failed", denial: {reason_code: "read_denied", resource: "private.txt"}}
        ]
      },
      expected: "read denied · private.txt"
    },
    {
      recovery: undefined,
      execution: {
        terminal_status: "rejected",
        attempts: [{status: "rejected", reason: "authority_compile"}]
      },
      expected: "Tool rejected: authority compile"
    }
  ])("prefers current structured failure details: $expected", ({recovery, execution, expected}) => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "structured", tool: "shell_read", arguments: {command: "inspect"}
      }),
      event(2, "tool.result", {
        call_id: "structured", output: "ordinary output", is_error: true, recovery, execution
      }),
      event(3, "command.execution", {
        call_id: "structured", command: "inspect", status: "failed", exit_code: 1
      })
    ]);
    expect(snapshot.nodes.get("tool-structured")).toMatchObject({
      state: "failed", errorSummary: expected, output: "ordinary output"
    });
  });

  it("does not reuse a resolved attempt denial or infer a reason from output", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "retry", tool: "exec_command", arguments: {command: "inspect"}
      }),
      event(2, "tool.result", {
        call_id: "retry", output: "package service", is_error: false,
        execution: {
          terminal_status: "succeeded",
          attempts: [
            {status: "failed", denial: {reason_code: "network_denied", resource: "old.example"}},
            {status: "succeeded"}
          ]
        }
      }),
      event(3, "command.execution", {call_id: "retry", command: "inspect", status: "failed"})
    ]);
    expect(snapshot.nodes.get("tool-retry")).toMatchObject({
      state: "failed",
      errorSummary: 'Tool failed · output: "package service"'
    });
  });

  it.each([
    {status: "timed_out", expected: "Command timed out"},
    {status: "canceled", expected: "Command canceled"},
    {status: "completed", expected: ""}
  ])("shows the actual $status command status and clears stale summaries", ({status, expected}) => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "status", tool: "exec_command", arguments: {command: "inspect"}
      }),
      event(2, "tool.result", {
        call_id: "status", output: "ordinary output", is_error: true
      }),
      event(3, "command.execution", {call_id: "status", command: "inspect", status, exit_code: 0})
    ]);
    expect(snapshot.nodes.get("tool-status")).toMatchObject({
      state: status === "completed" ? "completed" : "failed",
      errorSummary: expected,
      command: {exitCode: 0}
    });
  });

  it("keeps a command running when its initial tool result arrives after started", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "running", tool: "exec_command", arguments: {command: "inspect"}
      }),
      event(2, "command.execution", {call_id: "running", command: "inspect", status: "started"}),
      event(3, "tool.result", {call_id: "running", output: "working", is_error: false})
    ]);
    expect(snapshot.nodes.get("tool-running")).toMatchObject({
      state: "running", errorSummary: "", output: "working"
    });
  });

  it("projects file_apply patch content into the edit card", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "apply-1",
        tool: "file_apply",
        arguments: {
          changes: [{
            op: "edit",
            path: "main.go",
            old: "old\n",
            new: "new\n"
          }]
        }
      }),
      event(2, "tool.result", {
        call_id: "apply-1",
        tool: "file_apply",
        output: "modified main.go +1 -1",
        changes: [{path: "main.go", kind: "modified", added: 1, removed: 1}],
        is_error: false
      })
    ]);

    expect(snapshot.nodes.get("tool-apply-1")).toMatchObject({
      kind: "tool",
      variant: "diff",
      editPlan: {
        files: [{
          path: "main.go",
          before: "old\n",
          after: "new\n"
        }]
      }
    });
  });

  it("projects file_write content into the diff card", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "write-1",
        tool: "file_write",
        arguments: {
          path: "generated.txt",
          content: "first line\nsecond line\n"
        }
      }),
      event(2, "tool.result", {
        call_id: "write-1",
        tool: "file_write",
        output: "written",
        changes: [{path: "generated.txt", kind: "created", added: 2, removed: 0}],
        is_error: false
      })
    ]);

    expect(snapshot.nodes.get("tool-write-1")).toMatchObject({
      kind: "tool",
      variant: "diff",
      summary: "generated.txt · +2 -0",
      editPlan: {
        files: [{
          path: "generated.txt",
          after: "first line\nsecond line\n",
          beforeExists: false,
          afterExists: true
        }]
      }
    });
  });

  it("waits for an authoritative plan for file_apply write and move operations", () => {
    const snapshot = projectConversation([
      event(1, "tool.start", {
        call_id: "apply-write",
        tool: "file_apply",
        arguments: {
          changes: [{
            op: "write",
            path: "new.go",
            content: "package main\n"
          }]
        }
      }),
      event(2, "tool.start", {
        call_id: "apply-move",
        tool: "file_apply",
        arguments: {
          changes: [{
            op: "move",
            path: "old.go",
            to: "new.go"
          }]
        }
      })
    ]);

    const write = snapshot.nodes.get("tool-apply-write");
    const move = snapshot.nodes.get("tool-apply-move");
    expect(write?.kind).toBe("tool");
    expect(move?.kind).toBe("tool");
    if (write?.kind !== "tool" || move?.kind !== "tool") {
      throw new Error("file_apply calls were not projected as tools");
    }
    expect(write.editPlan).toBeUndefined();
    expect(move.editPlan).toBeUndefined();
  });

  it("groups child turn execution under its subagent", () => {
    const child = (sequence: number, kind: string, data: Record<string, unknown>) => ({
      ...event(sequence, kind, data),
      thread_id: "thread-agent-1",
      turn_id: "child-turn"
    });
    const snapshot = projectConversation([
      {
        ...event(1, "agent.spawned", {
          agent_id: "agent-1",
          role: "review",
          detail: {
            thread_id: "thread-agent-1",
            task_name: "Review persistence"
          }
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      },
      {
        ...event(2, "agent.status", {
          agent_id: "agent-1",
          status: "starting"
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      },
      {
        ...event(3, "agent.status", {
          agent_id: "agent-1",
          status: "running",
          message: "Reviewing persistence"
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      },
      {
        ...event(4, "agent.message", {
          body: {
            from: "agent-1",
            to: "parent",
            kind: "context",
            body: {task_name: "Review persistence"}
          }
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      },
      child(5, "turn.started", {
        display_prompt: '<task_capsule>{"task_name":"Review persistence"}</task_capsule>'
      }),
      child(6, "reasoning.delta", {
        sample_id: "sample-1",
        text: "Checking replay ownership"
      }),
      child(7, "tool.start", {
        call_id: "child-call",
        tool: "file_read",
        arguments: {path: "internal/persist/history/service.go"}
      }),
      child(8, "tool.result", {
        call_id: "child-call",
        tool: "file_read",
        output: "package history"
      }),
      child(9, "turn.completed", {text: "The replay filter needs session ownership."}),
      {
        ...event(10, "agent.status", {
          agent_id: "agent-1",
          status: "completed",
          message: "The replay filter needs session ownership."
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      },
      {
        ...event(11, "agent.message", {
          body: {
            from: "agent-1",
            to: "root",
            kind: "completion",
            body: {status: "completed"}
          }
        }),
        thread_id: "thread_external",
        turn_id: "turn_external"
      }
    ]);

    expect(snapshot.order).toEqual(["agent-agent-1"]);
    const agent = snapshot.nodes.get("agent-agent-1");
    expect(agent?.kind).toBe("agent");
    if (agent?.kind !== "agent") throw new Error("agent was not projected");
    expect(agent).toMatchObject({
      role: "review",
      taskName: "Review persistence",
      status: "completed",
      state: "completed",
      summary: "The replay filter needs session ownership."
    });
    expect(agent.activities.map(({title, state}) => ({title, state}))).toEqual([
      {title: "Started", state: "completed"},
      {title: "Thinking", state: "running"},
      {title: "Read", state: "completed"},
      {title: "Result", state: "completed"},
      {title: "Completed", state: "completed"},
      {title: "Result", state: "completed"}
    ]);
    expect(JSON.stringify(agent.activities)).not.toContain("task_capsule");
  });

  it("uses structured failure details for child tools without treating output as the cause", () => {
    const snapshot = projectConversation([
      event(1, "agent.spawned", {
        agent_id: "child", detail: {thread_id: "child-thread"}
      }),
      {
        ...event(2, "tool.start", {
          call_id: "child-probe", tool: "file_read", arguments: {path: "proto"}
        }),
        thread_id: "child-thread"
      },
      {
        ...event(3, "tool.result", {
          call_id: "child-probe", output: "ordinary output", is_error: true,
          recovery: {error_category: "file_not_found", path: "proto"}
        }),
        thread_id: "child-thread"
      }
    ]);
    const node = snapshot.nodes.get("agent-child");
    expect(node?.kind).toBe("agent");
    if (node?.kind !== "agent") throw new Error("agent was not projected");
    expect(node.activities.find((item) => item.callID === "child-probe")).toMatchObject({
      state: "failed", summary: "file not found · proto"
    });
  });

  it("projects failed agent reason codes and usage", () => {
    const snapshot = projectConversation([
      event(1, "agent.spawned", {
        agent_id: "agent-2",
        role: "review",
        detail: {task_name: "Audit paxos"}
      }),
      event(2, "agent.status", {
        agent_id: "agent-2",
        status: "failed",
        message: "token budget exhausted: projected 17698, limit 15000",
        reason_code: "budget_exhausted",
        detail: {
          result: {
            usage: {input_tokens: 0, output_tokens: 0}
          }
        }
      })
    ]);
    const agent = snapshot.nodes.get("agent-agent-2");
    expect(agent?.kind).toBe("agent");
    if (agent?.kind !== "agent") throw new Error("agent was not projected");
    expect(agent).toMatchObject({
      status: "failed",
      state: "failed",
      reasonCode: "budget_exhausted",
      usage: {inputTokens: 0, outputTokens: 0},
      summary: "token budget exhausted: projected 17698, limit 15000"
    });
  });
});

function serializable(snapshot: ReturnType<typeof projectConversation>) {
  return {
    order: snapshot.order,
    nodes: [...snapshot.nodes],
    activeTurnID: snapshot.activeTurnID,
    pendingApproval: snapshot.pendingApproval,
    pendingInput: snapshot.pendingInput
  };
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
    created_at: `2026-01-01T00:00:0${sequence}Z`,
    data
  };
}

function turnEvent(
  sequence: number,
  turnID: string,
  kind: string,
  data: Record<string, unknown>
): RuntimeEvent {
  return {
    ...event(sequence, kind, data),
    id: `${turnID}-event-${sequence}`,
    turn_id: turnID
  };
}
