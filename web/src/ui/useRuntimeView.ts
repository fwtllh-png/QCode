import {useCallback, useRef, useSyncExternalStore} from "react";
import {contentDeltaKinds, type ConversationNode} from "../projection/conversation";
import type {RuntimeEvent} from "../protocol";
import type {RuntimeClient, RuntimeSnapshot} from "../runtime/client";

type RuntimeStore = Pick<RuntimeClient, "subscribe" | "getSnapshot">;

const streamingNodeKinds: ReadonlySet<ConversationNode["kind"]> = new Set([
  "assistant",
  "reasoning",
  "tool",
  "agent"
]);

/**
 * The workbench view of the Runtime snapshot. A streaming frame that only
 * extends the content of existing transcript nodes keeps the previous view,
 * so the workbench tree is not reconciled for it. The node renderers follow
 * their own node through useLiveNode and event-log consumers follow the log
 * through useRuntimeEvents.
 */
export function useWorkbenchSnapshot(store: RuntimeStore): RuntimeSnapshot {
  const cache = useRef<{source: RuntimeSnapshot; view: RuntimeSnapshot}>(undefined);
  const read = useCallback(() => {
    const source = store.getSnapshot();
    const cached = cache.current;
    if (cached && (cached.source === source || isContentFrame(cached.source, source))) {
      cached.source = source;
      return cached.view;
    }
    cache.current = {source, view: source};
    return source;
  }, [store]);
  return useSyncExternalStore(store.subscribe, read, read);
}

/**
 * Reports whether `next` differs from `previous` only by streamed content:
 * the conversation structure is unchanged, every other field is identical,
 * and the event log grew only by content deltas.
 */
export function isContentFrame(previous: RuntimeSnapshot, next: RuntimeSnapshot): boolean {
  if (previous.conversation.structureRevision !== next.conversation.structureRevision) {
    return false;
  }
  for (const key of Object.keys(next) as (keyof RuntimeSnapshot)[]) {
    if (key === "events" || key === "conversation") continue;
    if (!Object.is(previous[key], next[key])) return false;
  }
  const before = previous.events;
  const after = next.events;
  if (after.length < before.length || after[0] !== before[0] ||
      after[before.length - 1] !== before.at(-1)) return false;
  for (let index = before.length; index < after.length; index += 1) {
    if (!contentDeltaKinds.has(after[index]!.kind)) return false;
  }
  return true;
}

/** The latest projection of a streaming transcript node. */
export function useLiveNode<T extends ConversationNode>(store: RuntimeStore, entry: T): T {
  const read = useCallback(() => {
    if (!streamingNodeKinds.has(entry.kind)) return entry;
    const node = store.getSnapshot().conversation.nodes.get(entry.id);
    return node?.kind === entry.kind ? node as T : entry;
  }, [store, entry]);
  return useSyncExternalStore(store.subscribe, read, read);
}

/** The full selected-session event log, including streamed deltas. */
export function useRuntimeEvents(store: RuntimeStore): readonly RuntimeEvent[] {
  const read = useCallback(() => store.getSnapshot().events, [store]);
  return useSyncExternalStore(store.subscribe, read, read);
}
