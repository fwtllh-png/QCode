import {ChevronDown, ChevronRight} from "lucide-react";
import {useEffect, useMemo, useState, type ReactNode} from "react";
import type {ConversationNode} from "../projection/conversation";
import {Collapse} from "./primitives/Collapse";

export function ExecutionStages({
  entries, revealEntryID, renderEntry
}: {
  entries: readonly ConversationNode[];
  revealEntryID?: string;
  renderEntry: (entry: ConversationNode) => ReactNode;
}) {
  const groups = useMemo(() => {
    const result: Array<{id: string; entries: ConversationNode[]; detail: boolean}> = [];
    for (const entry of entries) {
      const detail = entry.kind !== "commentary" &&
        entry.kind !== "assistant" && entry.kind !== "user";
      const last = result.at(-1);
      if (detail && last?.detail) last.entries.push(entry);
      else result.push({id: entry.id, entries: [entry], detail});
    }
    return result;
  }, [entries]);
  const grouped = entries.some((entry) => entry.kind === "commentary");
  // Keep the same owners before and after the first commentary arrives.
  // Adding a stage heading must not remount an already inspected tool.
  return <>{groups.map((group) => group.detail ? (
    <ExecutionStage
      key={group.id}
      entries={group.entries}
      grouped={grouped}
      revealEntryID={revealEntryID}
      renderEntry={renderEntry}
    />
  ) : renderEntry(group.entries[0]!))}</>;
}

function ExecutionStage({
  entries, grouped, revealEntryID, renderEntry
}: {
  entries: readonly ConversationNode[];
  grouped: boolean;
  revealEntryID?: string;
  renderEntry: (entry: ConversationNode) => ReactNode;
}) {
  const [open, setOpen] = useState(true);
  const reveal = entries.some((entry) => entry.id === revealEntryID);
  useEffect(() => {
    if (reveal) setOpen(true);
  }, [reveal, revealEntryID]);
  return (
    <div className="turnExecution">
      {grouped && <button
        type="button"
        className="turnExecutionToggle"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
      >
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        <span>Stage details</span>
        <small>{entries.length} {entries.length === 1 ? "step" : "steps"}</small>
      </button>}
      <Collapse open={!grouped || open}><div className="turnExecutionItems">{entries.map(renderEntry)}</div></Collapse>
    </div>
  );
}
