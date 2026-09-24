import {
  Bot,
  Check,
  ChevronLeft,
  ChevronRight,
  ChevronsDownUp,
  ChevronsUpDown,
  Clock3,
  Copy,
  FileDiff,
  Gauge,
  ListTree,
  MessageSquareText,
  RefreshCw,
  Search,
  Settings2,
  UserRound,
  Wrench,
  X
} from "lucide-react";
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent,
  type WheelEvent
} from "react";
import type {RuntimeEvent, TraceSnapshot} from "../protocol";
import {
  projectTrajectory,
  type TrajectoryRecord,
  type TrajectorySpan
} from "../projection/trajectory";
import {experience, trajectoryDOMBudget} from "./experience";
import "./Trajectory.css";

interface Props {
  events: readonly RuntimeEvent[];
  trace?: TraceSnapshot;
  tracePhase: "idle" | "loading" | "ready" | "unavailable";
  traceProblem?: string;
  hasEarlier: boolean;
  inspectCallID?: string;
  onInspectConsumed: () => void;
  onLoadEarlier: () => Promise<number>;
  onRetryTrace: () => Promise<void>;
  onOpenChat: (turnID: string, callID?: string, entryID?: string) => void;
}

type TimeRange = {start: number; end: number};
type Viewport = {start: number; end: number};

const compactTokens = new Intl.NumberFormat("en", {
  notation: "compact",
  maximumFractionDigits: 1
});
const exactTokens = new Intl.NumberFormat("en", {maximumFractionDigits: 0});

export function Trajectory({
  events,
  trace,
  tracePhase,
  traceProblem,
  hasEarlier,
  inspectCallID,
  onInspectConsumed,
  onLoadEarlier,
  onRetryTrace,
  onOpenChat
}: Props) {
  const projection = useMemo(
    () => projectTrajectory(events, trace),
    [events, trace]
  );
  const [duration, setDuration] = useState(true);
  const [turnsCollapsed, setTurnsCollapsed] = useState(false);
  const [callsCollapsed, setCallsCollapsed] = useState(false);
  const [query, setQuery] = useState("");
  const [selectedID, setSelectedID] = useState("");
  const [range, setRange] = useState<TimeRange>();
  const [viewport, setViewport] = useState<Viewport>();
  const ledgerRef = useRef<HTMLDivElement>(null);
  const rowRefs = useRef(new Map<string, HTMLButtonElement>());
  const selected = projection.records.find((record) => record.id === selectedID);
  const selectedSpan = projection.spans.find((span) => span.recordID === selectedID);
  const matchingIDs = useMemo(() => {
    const normalized = query.trim().toLocaleLowerCase();
    if (!normalized) return undefined;
    return new Set(
      projection.records
        .filter((record) => record.searchText.includes(normalized))
        .map((record) => record.id)
    );
  }, [projection.records, query]);
  const visibleRecords = useMemo(() => {
    const firstByTurn = new Set<string>();
    return projection.records.filter((record) => {
      if (matchingIDs && !matchingIDs.has(record.id)) return false;
      if (callsCollapsed && record.kind === "tool") return false;
      if (turnsCollapsed && record.turnID) {
        if (firstByTurn.has(record.turnID)) return false;
        firstByTurn.add(record.turnID);
      }
      return true;
    });
  }, [callsCollapsed, matchingIDs, projection.records, turnsCollapsed]);

  useEffect(() => {
    if (!inspectCallID) return;
    const record = projection.records.find(
      (candidate) => candidate.callID === inspectCallID
    );
    if (record) {
      setCallsCollapsed(false);
      setTurnsCollapsed(false);
      setSelectedID(record.id);
      requestAnimationFrame(() => {
        rowRefs.current.get(record.id)?.scrollIntoView({block: "center"});
      });
    }
    onInspectConsumed();
  }, [inspectCallID, onInspectConsumed, projection.records]);

  useEffect(() => {
    if (selectedID && !projection.records.some((record) => record.id === selectedID)) {
      setSelectedID("");
    }
  }, [projection.records, selectedID]);

  return (
    <section className="trajectory" aria-label="Execution trajectory">
      <div className="trajectoryToolbar">
        <div className="trajectoryModes" role="group" aria-label="Trajectory display">
        <button
          aria-pressed={duration}
          onClick={() => {
            setDuration((value) => !value);
            setRange(undefined);
            setViewport(undefined);
          }}
        >
          <Clock3 size={13} /> Duration
        </button>
        <button
          aria-pressed={turnsCollapsed}
          onClick={() => setTurnsCollapsed((value) => !value)}
        >
          {turnsCollapsed
            ? <ChevronsUpDown size={13} />
            : <ChevronsDownUp size={13} />}
          Turns
        </button>
        <button
          aria-pressed={callsCollapsed}
          onClick={() => setCallsCollapsed((value) => !value)}
        >
          <ListTree size={13} /> Calls
        </button>
        </div>
        <div className="trajectoryFilters">
        <label className="trajectorySearch">
          <Search size={13} />
          <span className="srOnly">Search trajectory</span>
          <input
            type="search"
            placeholder="Search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
        </label>
        {projection.prefixTokens !== undefined && (
          <div className="trajectoryMetrics" aria-label="Prefix metrics"
            title={`${exactTokens.format(projection.prefixTokens)} prefix tokens`}>
            <span>Prefix</span>
            <strong>{compactTokens.format(projection.prefixTokens)}</strong>
            <span>tokens</span>
          </div>
        )}
        </div>
      </div>
      <Timeline
        spans={projection.spans}
        records={projection.records}
        duration={duration}
        range={range}
        viewport={viewport}
        matchingIDs={matchingIDs}
        selectedID={selectedID}
        hasEarlier={hasEarlier}
        onLoadEarlier={onLoadEarlier}
        onRangeChange={setRange}
        onViewportChange={setViewport}
        onSelect={(recordID) => {
          setSelectedID(recordID);
          requestAnimationFrame(() => {
            rowRefs.current.get(recordID)?.scrollIntoView({block: "center"});
          });
        }}
      />
      {tracePhase === "loading" && (
        <div className="trajectoryNotice" role="status">Loading timing...</div>
      )}
      {tracePhase === "unavailable" && (
        <div className="trajectoryNotice" role="status">
          <span title={traceProblem}>Timing unavailable</span>
          <button onClick={() => void onRetryTrace()}>
            <RefreshCw size={12} /> Retry
          </button>
        </div>
      )}
      <div className="trajectorySplit">
        <Ledger
          records={visibleRecords}
          range={range}
          spans={projection.spans}
          selectedID={selectedID}
          rowRefs={rowRefs}
          scrollRef={ledgerRef}
          onSelect={setSelectedID}
        />
        {selected && (
          <RecordInspector
            record={selected}
            span={selectedSpan}
            records={visibleRecords}
            onClose={() => setSelectedID("")}
            onOpenChat={() => onOpenChat(
              selected.turnID,
              selected.callID || undefined,
              selected.id
            )}
            onSelect={(id) => {
              setSelectedID(id);
              requestAnimationFrame(() => {
                rowRefs.current.get(id)?.scrollIntoView({block: "center"});
              });
            }}
          />
        )}
      </div>
    </section>
  );
}

function Timeline({
  spans,
  records,
  duration,
  range,
  viewport,
  matchingIDs,
  selectedID,
  hasEarlier,
  onLoadEarlier,
  onRangeChange,
  onViewportChange,
  onSelect
}: {
  spans: readonly TrajectorySpan[];
  records: readonly TrajectoryRecord[];
  duration: boolean;
  range?: TimeRange;
  viewport?: Viewport;
  matchingIDs?: ReadonlySet<string>;
  selectedID: string;
  hasEarlier: boolean;
  onLoadEarlier: () => Promise<number>;
  onRangeChange: (range?: TimeRange) => void;
  onViewportChange: (range?: Viewport) => void;
  onSelect: (recordID: string) => void;
}) {
  const trackRef = useRef<HTMLDivElement>(null);
  const drag = useRef<{pointerID: number; anchor: number; moved: boolean}>();
  const visualFrame = useRef<number>();
  const pendingVisual = useRef<
    | {kind: "range"; value?: TimeRange}
    | {kind: "viewport"; value?: Viewport}
  >();
  const flushVisual = () => {
    visualFrame.current = undefined;
    const pending = pendingVisual.current;
    pendingVisual.current = undefined;
    if (pending?.kind === "range") onRangeChange(pending.value);
    if (pending?.kind === "viewport") onViewportChange(pending.value);
  };
  const scheduleVisual = (
    pending:
      | {kind: "range"; value?: TimeRange}
      | {kind: "viewport"; value?: Viewport}
  ) => {
    pendingVisual.current = pending;
    if (visualFrame.current === undefined) {
      visualFrame.current = requestAnimationFrame(flushVisual);
    }
  };
  useEffect(() => () => {
    if (visualFrame.current !== undefined) {
      cancelAnimationFrame(visualFrame.current);
    }
  }, []);
  const domain = useMemo(() => {
    if (spans.length === 0) return {start: 0, end: 1};
    const start = Math.min(...spans.map((span) => span.startedAt));
    const end = Math.max(...spans.map(
      (span) => span.endedAt ?? span.startedAt + (span.durationMS ?? 1)
    ));
    return {start, end: Math.max(start + 1, end)};
  }, [spans]);
  const visible = viewport ?? domain;
  const positioned = useMemo(() => spans.map((span, index) => {
    const start = duration ? span.startedAt : index;
    const end = duration
      ? span.endedAt ?? span.startedAt + (span.durationMS ?? 1)
      : index + 1;
    const scaleStart = duration ? visible.start : 0;
    const scaleEnd = duration ? visible.end : Math.max(1, spans.length);
    return {
      span,
      left: (start - scaleStart) / Math.max(1, scaleEnd - scaleStart),
      width: (end - start) / Math.max(1, scaleEnd - scaleStart)
    };
  }), [duration, spans, visible]);
  const timeAt = (clientX: number) => {
    const rect = trackRef.current?.getBoundingClientRect();
    const fraction = rect
      ? Math.min(1, Math.max(0, (clientX - rect.left) / Math.max(1, rect.width)))
      : 0;
    return visible.start + fraction * (visible.end - visible.start);
  };
  const nearest = (clientX: number) => {
    const value = timeAt(clientX);
    return spans.reduce<TrajectorySpan | undefined>((best, span) => {
      if (!best) return span;
      return Math.abs(span.startedAt - value) < Math.abs(best.startedAt - value)
        ? span
        : best;
    }, undefined);
  };
  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    if (!duration) return;
    drag.current = {
      pointerID: event.pointerId,
      anchor: timeAt(event.clientX),
      moved: false
    };
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    if (!drag.current || drag.current.pointerID !== event.pointerId) return;
    const current = timeAt(event.clientX);
    drag.current.moved = drag.current.moved ||
      Math.abs(current - drag.current.anchor) >
        (domain.end - domain.start) * experience.trajectory.dragThresholdFraction;
    if (drag.current.moved) {
      scheduleVisual({kind: "range", value: {
        start: Math.min(drag.current.anchor, current),
        end: Math.max(drag.current.anchor, current)
      }});
    }
  };
  const onPointerUp = (event: PointerEvent<HTMLDivElement>) => {
    const active = drag.current;
    drag.current = undefined;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    if (visualFrame.current !== undefined) {
      cancelAnimationFrame(visualFrame.current);
      flushVisual();
    }
    if (!active?.moved) {
      const record = nearest(event.clientX);
      if (record) onSelect(record.recordID);
    }
  };
  const onWheel = (event: WheelEvent<HTMLDivElement>) => {
    if (!duration) return;
    event.preventDefault();
    const full = domain.end - domain.start;
    const current = visible.end - visible.start;
    const rect = event.currentTarget.getBoundingClientRect();
    const fraction = Math.min(
      1,
      Math.max(0, (event.clientX - rect.left) / Math.max(1, rect.width))
    );
    if (event.shiftKey || Math.abs(event.deltaX) > Math.abs(event.deltaY)) {
      const shift = (event.deltaX || event.deltaY) / Math.max(1, rect.width) * current;
      const start = Math.min(
        domain.end - current,
        Math.max(domain.start, visible.start + shift)
      );
      scheduleVisual({kind: "viewport", value: {start, end: start + current}});
      return;
    }
    const nextDuration = Math.min(
      full,
      Math.max(
        full / Math.max(experience.trajectory.minimumZoomOperations, spans.length),
        current * (
          event.deltaY > 0
            ? experience.trajectory.zoomOutFactor
            : experience.trajectory.zoomInFactor
        )
      )
    );
    if (nextDuration >= full) {
      scheduleVisual({kind: "viewport"});
      return;
    }
    const anchor = visible.start + fraction * current;
    const start = Math.min(
      domain.end - nextDuration,
      Math.max(domain.start, anchor - nextDuration * fraction)
    );
    scheduleVisual({
      kind: "viewport",
      value: {start, end: start + nextDuration}
    });
  };
  return (
    <div className="trajectoryTimeline">
      <div className="timelineLabels" aria-hidden="true">
        <span>Input</span><span>Model</span><span>Tools</span>
      </div>
      <div
        className="timelineTrack"
        ref={trackRef}
        tabIndex={0}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onWheel={onWheel}
        onDoubleClick={() => {
          onRangeChange(undefined);
          onViewportChange(undefined);
        }}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            onRangeChange(undefined);
            onViewportChange(undefined);
          }
        }}
      >
        {hasEarlier && (
          <button
            className="timelineEarlier"
            aria-label="Load earlier trajectory"
            onClick={(event) => {
              event.stopPropagation();
              void onLoadEarlier();
            }}
          >
            ...
          </button>
        )}
        {positioned.map(({span, left, width}) => (
          <button
            key={span.id}
            className="timelineSpan"
            data-lane={span.lane}
            data-failed={span.status === "error" || undefined}
            data-selected={span.recordID === selectedID || undefined}
            data-search-match={
              matchingIDs ? matchingIDs.has(span.recordID) : undefined
            }
            title={span.title}
            aria-label={span.title}
            style={{
              "--ch-span-left": `${left * 100}%`,
              "--ch-span-width": `${width * 100}%`
            } as CSSProperties}
            onPointerDown={(event) => event.stopPropagation()}
            onClick={() => onSelect(span.recordID)}
          />
        ))}
        {range && (
          <span
            className="timelineRange"
            style={{
              "--ch-range-left": `${
                (range.start - visible.start) /
                Math.max(1, visible.end - visible.start) * 100
              }%`,
              "--ch-range-width": `${
                (range.end - range.start) /
                Math.max(1, visible.end - visible.start) * 100
              }%`
            } as CSSProperties}
          />
        )}
        {spans.length === 0 && (
          <span className="timelineEmpty">
            {records.length === 0 ? "No events" : "Timing unavailable"}
          </span>
        )}
      </div>
    </div>
  );
}

function Ledger({
  records,
  spans,
  range,
  selectedID,
  rowRefs,
  scrollRef,
  onSelect
}: {
  records: readonly TrajectoryRecord[];
  spans: readonly TrajectorySpan[];
  range?: TimeRange;
  selectedID: string;
  rowRefs: React.MutableRefObject<Map<string, HTMLButtonElement>>;
  scrollRef: React.RefObject<HTMLDivElement>;
  onSelect: (id: string) => void;
}) {
  const [viewport, setViewport] = useState({
    height: experience.trajectory.initialViewportHeight,
    top: 0
  });
  const budget = trajectoryDOMBudget(viewport.height);
  const virtual = records.length > budget;
  const overscan = Math.ceil(
    viewport.height / experience.layout.trajectoryRow *
    experience.trajectory.overscanViewports
  );
  const start = virtual
    ? Math.max(
      0,
      Math.floor(viewport.top / experience.layout.trajectoryRow) - overscan
    )
    : 0;
  const end = virtual ? Math.min(records.length, start + budget) : records.length;
  const visible = records.slice(start, end);
  const timing = new Map(spans.map((span) => [span.recordID, span]));
  const turnNumbers = new Map<string, number>();
  for (const record of records) {
    if (record.turnID && !turnNumbers.has(record.turnID)) {
      turnNumbers.set(record.turnID, turnNumbers.size + 1);
    }
  }
  let previousTurn = "";
  return (
    <div
      className="trajectoryLedger"
      ref={scrollRef}
      onScroll={(event) => setViewport({
        height: event.currentTarget.clientHeight,
        top: event.currentTarget.scrollTop
      })}
    >
      <div className="ledgerHeader" aria-hidden="true">
        <span>Event</span><span>Content</span>
      </div>
      {start > 0 && (
        <div style={{height: start * experience.layout.trajectoryRow}} />
      )}
      {visible.map((record) => {
        const turnStart = record.turnID !== previousTurn;
        previousTurn = record.turnID;
        const span = timing.get(record.id);
        const outside = range && span
          ? span.startedAt > range.end ||
            (span.endedAt ?? span.startedAt) < range.start
          : false;
        return (
          <button
            className="ledgerRow"
            key={record.id}
            ref={(node) => {
              if (node) rowRefs.current.set(record.id, node);
              else rowRefs.current.delete(record.id);
            }}
            data-kind={record.kind}
            data-turn-start={turnStart || undefined}
            data-selected={record.id === selectedID || undefined}
            data-outside-range={outside || undefined}
            onClick={() => onSelect(record.id)}
          >
            <span className="ledgerKind">
              {turnStart && record.turnID && (
                <b title={record.turnID}>T{turnNumbers.get(record.turnID)}</b>
              )}
              <RecordKindIcon record={record} />
              <span>{record.label}</span>
            </span>
            <span className="ledgerSummary">{record.summary}</span>
            {span?.durationMS !== undefined && (
              <span className="ledgerDuration">{formatMilliseconds(span.durationMS)}</span>
            )}
          </button>
        );
      })}
      {end < records.length && (
        <div style={{height: (records.length - end) * experience.layout.trajectoryRow}} />
      )}
      {records.length === 0 && (
        <div className="trajectoryEmpty">No trajectory events</div>
      )}
    </div>
  );
}

export function RecordInspector({
  record,
  span,
  records,
  onClose,
  onOpenChat,
  onSelect
}: {
  record: TrajectoryRecord;
  span?: TrajectorySpan;
  records: readonly TrajectoryRecord[];
  onClose: () => void;
  onOpenChat: () => void;
  onSelect: (id: string) => void;
}) {
  const index = records.findIndex((candidate) => candidate.id === record.id);
  const tabs = inspectorTabs(record);
  const [activeTab, setActiveTab] = useState(tabs[0]?.id ?? "summary");
  const [width, setWidth] = useState(() => storedInspectorWidth());
  const drag = useRef<{pointerID: number; x: number; width: number}>();
  useEffect(() => {
    setActiveTab(inspectorTabs(record)[0]?.id ?? "summary");
  }, [record.id]);
  const active = tabs.some((tab) => tab.id === activeTab)
    ? activeTab
    : tabs[0]?.id ?? "summary";
  return (
    <aside
      className="recordInspector"
      aria-label="Record inspector"
      style={{"--ch-inspector-width": `${width}px`} as CSSProperties}
    >
      <div
        className="recordInspectorResize"
        role="separator"
        aria-label="Resize record inspector"
        aria-orientation="vertical"
        aria-valuemin={320}
        aria-valuemax={720}
        aria-valuenow={width}
        tabIndex={0}
        onKeyDown={(event) => {
          if (event.key === "ArrowLeft") setInspectorWidth(width + 16, setWidth);
          if (event.key === "ArrowRight") setInspectorWidth(width - 16, setWidth);
        }}
        onPointerDown={(event) => {
          drag.current = {pointerID: event.pointerId, x: event.clientX, width};
          event.currentTarget.setPointerCapture(event.pointerId);
        }}
        onPointerMove={(event) => {
          if (drag.current?.pointerID !== event.pointerId) return;
          setInspectorWidth(
            drag.current.width + drag.current.x - event.clientX,
            setWidth
          );
        }}
        onPointerUp={(event) => {
          if (drag.current?.pointerID !== event.pointerId) return;
          drag.current = undefined;
          event.currentTarget.releasePointerCapture(event.pointerId);
        }}
      />
      <header>
        <span>
          <strong>{record.label}</strong>
          <small title={record.summary}>{record.summary}</small>
        </span>
        <div>
          {record.turnID && (
            <button aria-label="Show in chat" onClick={onOpenChat}>
              <MessageSquareText size={14} />
            </button>
          )}
          <button
            aria-label="Previous record"
            disabled={index <= 0}
            onClick={() => onSelect(records[index - 1]!.id)}
          >
            <ChevronLeft size={14} />
          </button>
          <button
            aria-label="Next record"
            disabled={index < 0 || index >= records.length - 1}
            onClick={() => onSelect(records[index + 1]!.id)}
          >
            <ChevronRight size={14} />
          </button>
          <button aria-label="Close record inspector" onClick={onClose}>
            <X size={14} />
          </button>
        </div>
      </header>
      <div className="inspectorTabs" role="tablist" aria-label="Record details">
        {tabs.map((tab) => (
          <button
            key={tab.id}
            type="button"
            role="tab"
            aria-selected={active === tab.id}
            onClick={() => setActiveTab(tab.id)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      <div className="inspectorBody">
        <InspectorContent record={record} span={span} tab={active} />
      </div>
    </aside>
  );
}

type InspectorTab = "summary" | "input" | "output" | "usage" | "timing" | "changes" | "raw";

function inspectorTabs(record: TrajectoryRecord): readonly {
  id: InspectorTab;
  label: string;
}[] {
  return [
    {id: "summary", label: "Summary"},
    ...(record.input === undefined ? [] : [{id: "input", label: "Input"} as const]),
    ...(record.output === undefined ? [] : [{id: "output", label: "Output"} as const]),
    ...(record.usage === undefined ? [] : [{id: "usage", label: "Usage"} as const]),
    ...(record.timing === undefined ? [] : [{id: "timing", label: "Timing"} as const]),
    ...(record.changes?.length ? [{id: "changes", label: "Changes"} as const] : []),
    {id: "raw", label: "Raw"}
  ];
}

function InspectorContent({
  record,
  span,
  tab
}: {
  record: TrajectoryRecord;
  span?: TrajectorySpan;
  tab: InspectorTab;
}) {
  if (tab === "summary") {
    return (
      <>
        <dl className="inspectorFacts">
          <div><dt>Status</dt><dd data-failed={record.failed || undefined}>
            {record.failed ? "Failed" : "Recorded"}
          </dd></div>
          <div><dt>Started</dt><dd>{formatTimestamp(record.createdAt)}</dd></div>
          <div><dt>Duration</dt><dd>
            {span?.durationMS === undefined
              ? "Not recorded"
              : formatMilliseconds(span.durationMS)}
          </dd></div>
          {span?.ttftMS !== undefined && (
            <div><dt>TTFT</dt><dd>{formatMilliseconds(span.ttftMS)}</dd></div>
          )}
          <div><dt>Turn</dt><dd>{record.turnID || "None"}</dd></div>
          <div><dt>Call</dt><dd>{record.callID || "None"}</dd></div>
          <div><dt>Item</dt><dd>{record.itemID || "None"}</dd></div>
          <div><dt>Sequence</dt><dd>{record.sequence}</dd></div>
        </dl>
        <div className="inspectorPreview">
          <span>Preview</span>
          <p>{record.summary}</p>
        </div>
      </>
    );
  }
  const value = tab === "input"
    ? record.input
    : tab === "output"
      ? record.output
      : tab === "usage"
        ? record.usage
        : tab === "timing"
          ? record.timing
          : tab === "changes"
            ? record.changes
            : record.raw;
  const text = pretty(value);
  return (
    <div className="inspectorCode">
      <button
        type="button"
        aria-label={`Copy ${tab}`}
        onClick={() => {
          void navigator.clipboard.writeText(text).catch(() => {});
        }}
      >
        <Copy size={13} /> Copy
      </button>
      <pre>{text}</pre>
    </div>
  );
}

function RecordKindIcon({record}: {record: TrajectoryRecord}) {
  switch (record.kind) {
    case "user":
      return <UserRound size={13} />;
    case "assistant":
      return <Bot size={13} />;
    case "tool":
      return <Wrench size={13} />;
    case "verification":
      return <Check size={13} />;
    case "receipt":
      return <Gauge size={13} />;
    case "context":
      return <FileDiff size={13} />;
    default:
      return <Settings2 size={13} />;
  }
}

function storedInspectorWidth(): number {
  try {
    const value = Number(window.localStorage?.getItem("ch.trajectory.inspector.width"));
    return Number.isFinite(value) ? Math.min(720, Math.max(320, value)) : 420;
  } catch {
    return 420;
  }
}

function setInspectorWidth(
  value: number,
  apply: (value: number) => void
): void {
  const width = Math.min(720, Math.max(320, Math.round(value)));
  apply(width);
  try {
    window.localStorage?.setItem("ch.trajectory.inspector.width", String(width));
  } catch {
    // The panel remains usable when browser preferences are unavailable.
  }
}

function formatTimestamp(value: string): string {
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) return "Not recorded";
  return new Date(timestamp).toLocaleString();
}

function pretty(value: unknown): string {
  if (typeof value === "string") return value;
  return JSON.stringify(value, null, 2);
}

function formatMilliseconds(value: number): string {
  return value < 1_000
    ? `${Math.round(value)} ms`
    : `${(value / 1_000).toFixed(value < 10_000 ? 2 : 1)} s`;
}
