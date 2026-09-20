import {useMemo, useRef} from "react";
import type {RuntimeEvent} from "../protocol";

// Only cache presentation metadata. The complete event ledger still feeds
// conversation projection, history, and trajectory.
export function usePresentationEvents(
  events: readonly RuntimeEvent[]
): readonly RuntimeEvent[] {
  const cache = useRef<{
    source: readonly RuntimeEvent[];
    selected: readonly RuntimeEvent[];
  }>({source: [], selected: []});
  return useMemo(() => {
    const previous = cache.current;
    const append = events.length >= previous.source.length &&
      events[0] === previous.source[0] &&
      events[previous.source.length - 1] === previous.source.at(-1);
    const start = append ? previous.source.length : 0;
    const added: RuntimeEvent[] = [];
    for (let index = start; index < events.length; index += 1) {
      const event = events[index]!;
      if (event.kind !== "output.delta" && event.kind !== "output.draft" &&
          event.kind !== "output.discarded" &&
          event.kind !== "reasoning.delta" &&
          event.kind !== "tool.output") added.push(event);
    }
    const selected = append && added.length === 0
      ? previous.selected
      : [...(append ? previous.selected : []), ...added];
    cache.current = {source: events, selected};
    return selected;
  }, [events]);
}
