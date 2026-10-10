import {describe, expect, it} from "vitest";

import type {ConversationNode} from "../projection/conversation";
import {
  adjacentQuestion,
  projectConversationNavigation,
  questionPosition,
  searchConversationNavigation,
  transcriptPageForEntry
} from "./conversationNavigation";

describe("conversation navigation", () => {
  const entries: ConversationNode[] = [
    {
      id: "user-1",
      kind: "user",
      turnID: "turn-a",
      sequence: 1,
      text: "Inspect the parser",
      images: []
    },
    {
      id: "tool-1",
      kind: "tool",
      turnID: "turn-a",
      sequence: 2,
      callID: "call-read",
      tool: "read_file",
      variant: "read",
      title: "Read parser.go",
      summary: "Read the parser implementation",
      state: "completed",
      arguments: {path: "internal/parser.go"},
      output: "package parser",
      errorSummary: "",
      truncated: false,
      changes: []
    },
    {
      id: "user-2",
      kind: "user",
      turnID: "turn-b",
      sequence: 3,
      text: "Update the tests",
      images: []
    },
    {
      id: "files-2",
      kind: "deliverables",
      turnID: "turn-b",
      sequence: 4,
      files: [{
        path: "internal/parser_test.go",
        tool: "file_edit",
        kind: "modified",
        added: 8,
        removed: 2,
        summary: "Added parser coverage",
        callID: "call-edit",
        stale: false
      }]
    }
  ];

  it("indexes turns, questions, tools, and structured file references", () => {
    const items = projectConversationNavigation(entries);

    expect(items.filter((item) => item.kind === "turn")).toMatchObject([
      {id: "turn:turn-a", entryID: "user-1", turnNumber: 1},
      {id: "turn:turn-b", entryID: "user-2", turnNumber: 2}
    ]);
    expect(items.filter((item) => item.kind === "question")).toHaveLength(2);
    expect(items.filter((item) => item.kind === "tool")).toMatchObject([
      {id: "tool:tool-1", callID: "call-read"}
    ]);
    expect(items.filter((item) => item.kind === "file")).toMatchObject([
      {entryID: "tool-1", path: "internal/parser.go"},
      {entryID: "files-2", path: "internal/parser_test.go"}
    ]);
  });

  it("searches normalized labels and filters by result kind", () => {
    const items = projectConversationNavigation(entries);

    expect(searchConversationNavigation(items, "PARSER TEST", "file"))
      .toMatchObject([{path: "internal/parser_test.go"}]);
    expect(searchConversationNavigation(items, "read parser", "tool"))
      .toMatchObject([{callID: "call-read"}]);
    expect(searchConversationNavigation(items, "missing")).toEqual([]);
  });

  it("moves between questions from a stable entry identity", () => {
    const items = projectConversationNavigation(entries);

    expect(questionPosition(items, "tool-1")).toMatchObject({
      index: 0,
      total: 2,
      item: {entryID: "user-1"}
    });
    expect(adjacentQuestion(items, "tool-1", 1)).toMatchObject({
      entryID: "user-2"
    });
    expect(adjacentQuestion(items, "user-2", 1)).toBeUndefined();
    expect(adjacentQuestion(items, "user-2", -1)).toMatchObject({
      entryID: "user-1"
    });
    expect(questionPosition(items, "assistant-not-indexed", 2)).toMatchObject({
      index: 1,
      total: 2,
      item: {entryID: "user-2"}
    });
  });

  it("maps stable entry identities into overlapping bounded transcript pages", () => {
    const longEntries = Array.from({length: 500}, (_, index): ConversationNode => ({
      id: `user-${index}`,
      kind: "user",
      turnID: `turn-${index}`,
      sequence: index,
      text: `Question ${index}`,
      images: []
    }));

    expect(transcriptPageForEntry(longEntries, "user-499", 200, 168)).toBe(0);
    expect(transcriptPageForEntry(longEntries, "user-299", 200, 168)).toBe(1);
    expect(transcriptPageForEntry(longEntries, "user-99", 200, 168)).toBe(2);
    expect(transcriptPageForEntry(longEntries, "missing", 200, 168))
      .toBeUndefined();
  });
});
