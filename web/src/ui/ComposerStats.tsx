import {X} from "lucide-react";
import {useEffect, useId, useLayoutEffect, useRef, useState} from "react";
import type {UsageRollup} from "../protocol";
import type {ContextAttribution} from "./ConversationChrome";

interface Props {
  attribution?: ContextAttribution;
  capacity?: number;
  receipt?: Readonly<Record<string, unknown>>;
  usage?: UsageRollup;
  running: boolean;
  previous: boolean;
  terminal?: string;
}

const exactCount = new Intl.NumberFormat("en-US");
const tokenCount = new Intl.NumberFormat("en-US", {maximumFractionDigits: 1});

export function ComposerStats({attribution, capacity, receipt, usage, running, previous, terminal}: Props) {
  const [openMode, setOpenMode] = useState<"hover" | "activated" | null>(null);
  const open = openMode !== null;
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const panelID = useId();

  useLayoutEffect(() => {
    if (!open) return;
    const root = rootRef.current!;
    const panel = panelRef.current!;
    const position = () => {
      const anchor = root.getBoundingClientRect();
      const width = panel.getBoundingClientRect().width;
      const left = Math.max(12, Math.min(anchor.right - width, window.innerWidth - width - 12));
      root.style.setProperty("--stats-right", `${anchor.right - left - width}px`);
      root.style.setProperty("--stats-height", `${Math.max(0, anchor.top - 20)}px`);
    };
    position();
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(position);
    observer?.observe(root.closest(".composer") ?? root);
    window.addEventListener("resize", position);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", position);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    if (openMode === "activated") panelRef.current?.focus({preventScroll: true});
    const onPointerDown = (event: PointerEvent) => {
      if (event.target instanceof Node && !rootRef.current?.contains(event.target)) {
        setOpenMode(null);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      setOpenMode(null);
      if (rootRef.current?.contains(document.activeElement)) {
        triggerRef.current?.focus({preventScroll: true});
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open, openMode]);

  const budget = object(receipt?.context_budget);
  const used = number(attribution?.estimatedTokens) ?? number(budget?.active_tokens);
  const maximum = number(capacity) ?? number(budget?.max_context_tokens);
  const share = used !== undefined && maximum !== undefined && maximum > 0
    ? used / maximum : undefined;
  const percent = share === undefined ? undefined : Math.round(share * 100);
  const fill = share === undefined ? 0 : Math.min(1, share) * 100;
  const contextRows = attribution ? [
    {label: "Stable / system", value: attribution.stableTokens, tone: "stable"},
    {label: "Tools", value: attribution.toolTokens, tone: "tools"},
    {label: "Messages", value: attribution.messageTokens, tone: "messages"},
    {label: "Provider framing", value: attribution.framingTokens, tone: "framing"}
  ] : [];
  const measured = receipt?.measurement_recorded !== false;
  const latency = object(receipt?.latency);
  const input = measured ? number(receipt?.input_tokens) : undefined;
  const output = measured ? number(receipt?.output_tokens) : undefined;
  const total = input !== undefined && output !== undefined ? input + output : undefined;
  const cached = measured ? number(receipt?.cached_tokens) : undefined;
  const cacheShare = cached !== undefined && input !== undefined && input > 0 && cached <= input
    ? `${tokenCount.format(cached / input * 100)}%`
    : undefined;
  const execution = object(receipt?.tool_execution);
  // Failed is a subset of these categories, not another set of calls.
  const tools = measured && receipt && Object.hasOwn(receipt, "tool_execution")
    ? ["business", "control", "verification"].reduce(
      (sum, kind) => sum + (number(execution?.[kind]) ?? 0), 0
    )
    : undefined;
  const models = Array.isArray(receipt?.routes)
    ? [...new Set(receipt.routes.flatMap((route) => {
      const model = object(route)?.model;
      return typeof model === "string" && model ? [model] : [];
    }))]
    : [];
  const label = previous ? "Previous turn" : "Latest turn";
  const status = terminal === "turn.completed" ? "Completed"
    : terminal === "turn.failed" ? "Failed"
      : terminal === "turn.canceled" ? "Canceled" : "Recorded";
  const activity = usage?.activity;
  return (
    <div
      className="contextMeterRoot"
      ref={rootRef}
      onPointerLeave={() => {
        if (openMode === "hover" && !panelRef.current?.contains(document.activeElement)) {
          setOpenMode(null);
        }
      }}
      onBlur={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setOpenMode(null);
      }}
    >
      <button
        type="button"
        className="contextMeter"
        ref={triggerRef}
        aria-label={`${percent === undefined ? "Context usage unavailable" : `${percent}% of context used`}. Run statistics`}
        aria-expanded={open}
        aria-controls={open ? panelID : undefined}
        aria-haspopup="dialog"
        onPointerEnter={(event) => {
          if (event.pointerType === "mouse") setOpenMode((mode) => mode ?? "hover");
        }}
        onClick={() => setOpenMode((mode) => mode === "activated" ? null : "activated")}
      >
        <svg viewBox="0 0 16 16" aria-hidden="true">
          <circle className="contextTrack" cx="8" cy="8" r="6" />
          <circle className="contextFill" cx="8" cy="8" r="6"
            pathLength="100" strokeDasharray={`${fill} 100`} transform="rotate(-90 8 8)" />
        </svg>
      </button>
      {open && (
        <div className="composerStatsPopover">
        <div
          id={panelID}
          className="composerStatsPanel"
          role="dialog"
          aria-label="Run statistics details"
          tabIndex={-1}
          ref={panelRef}
        >
          <section aria-label="Context usage">
            <div className="composerStatsHeading">
              <h3>Context used</h3>
              <span>{running ? "Running · " : ""}{percent === undefined ? "Not available" : `${percent}%`}</span>
            </div>
            <p className="composerStatsContextTotal">
              {used === undefined ? "—" : `${attribution ? "~" : ""}${compactTokens(used)}`}
              {" / "}{maximum ? compactTokens(maximum) : "—"} tokens
            </p>
            <div className="contextBar" aria-hidden="true">
              {contextRows.length === 0
                ? <span style={{width: `${fill}%`}} />
                : contextRows.filter((row) => row.value > 0).map((row) => (
                  <span key={row.tone} data-tone={row.tone}
                    style={{width: `${used ? fill * row.value / used : 0}%`}} />
                ))}
            </div>
            {contextRows.length > 0 && <dl className="contextRows">
              {contextRows.map((row) => <div key={row.tone}>
                <dt><span data-tone={row.tone} aria-hidden="true" />{row.label}</dt>
                <dd>~{compactTokens(row.value)}</dd>
              </div>)}
            </dl>}
            <p className="composerStatsNote">
              {attribution ? "Last model sample · estimated context"
                : used !== undefined ? "Last recorded context" : "Context usage has not been recorded yet."}
            </p>
          </section>
          {receipt ? <section aria-label={`${label} details`}>
            <header>
              <div>
                <strong>{label}</strong>
                <p className="composerStatsModel">{models.join(" · ") || "Model not recorded"}</p>
              </div>
              <span className="composerStatsStatus">{status}</span>
            </header>
            <dl className="composerStatsTotal">
              <Stat label="Total tokens" value={count(total)} />
            </dl>
            <div className="composerStatsTokenBar" aria-hidden="true">
              {total !== undefined && total > 0 && <>
                <span data-token="input" style={{width: `${input! / total * 100}%`}} />
                <span data-token="output" style={{width: `${output! / total * 100}%`}} />
              </>}
            </div>
            <dl className="composerStatsTokens">
              <Stat label="Input" value={count(input)} tone="input" />
              <Stat label="Output" value={count(output)} tone="output" />
              <Stat label="↳ of which reasoning" value={count(measured ? receipt.reasoning_tokens : undefined)} />
            </dl>
            <dl className="composerStatsCache">
              <Stat label="Cached input" value={cached === undefined ? "Not reported" : count(cached)} />
              <Stat label="Cache share of input" value={cacheShare ?? "—"} />
            </dl>
            <dl className="composerStatsMetrics">
              <Stat label="Elapsed" value={duration(latency?.total_ms)} />
              <Stat label="First response" value={duration(latency?.first_token_ms)} />
              <Stat label="Model time" value={duration(latency?.provider_ms)} />
              <Stat label="Tool time (sum)" value={duration(latency?.tool_ms)} />
              <Stat label="Tool calls (settled)" value={count(tools)} />
              <Stat label="Cost" value={cost(receipt.cost_microunits, receipt.cost_known)} />
            </dl>
          </section> : <p className="composerStatsNote">No turn receipt yet.</p>}
          <section aria-label="Session totals">
            <div className="composerStatsHeading"><h3>Session totals</h3><span>Including subagents</span></div>
            <dl>
              <Stat label="Turns started" value={count(activity?.turns)} />
              {activity && <Stat label="Completed / failed / canceled"
                value={`${count(activity.completed)} / ${count(activity.failed)} / ${count(activity.canceled)}`} />}
              <Stat label="Tool calls (settled)" value={count(activity?.tool_calls)} />
              <Stat label="Model calls with usage" value={count(usage?.calls)} />
              <Stat label="Total tokens" value={count(usage?.total_tokens)} />
              <Stat label="Cost" value={usageCost(usage)} />
            </dl>
          </section>
          <footer className="composerStatsFooter">
            <details>
              <summary>About these numbers</summary>
              <p className="composerStatsNote">The ring shows context used / model context capacity from the latest available sample; unsent text is excluded. Total turn tokens = input + output across model calls. The input/output bar shows their share of that total. Reasoning is part of output; cache is part of input. 1K = 1,024 tokens.</p>
              <p className="composerStatsNote">First response includes text, reasoning or tool-call fragments. Parallel tool times are summed and include approval waits; times do not add up to elapsed time.</p>
              {receipt && <dl>
                <Stat label="Approval wait" value={duration(latency?.approval_wait_ms)} />
                <Stat label="Verification" value={duration(latency?.verify_ms)} />
              </dl>}
              <p className="composerStatsNote">Session totals cover all history, including subagents, failed and canceled turns. Tool counts include control calls and failures; unfinished calls are excluded. Usage includes auxiliary model calls.{running ? " Refreshes when the turn ends." : ""}</p>
            </details>
            <button type="button" aria-label="Close statistics" onClick={() => {
              setOpenMode(null);
              triggerRef.current?.focus({preventScroll: true});
            }}><X size={14} aria-hidden="true" /></button>
          </footer>
        </div>
        </div>
      )}
    </div>
  );
}

function Stat({label, value, tone}: {label: string; value: string; tone?: "input" | "output"}) {
  return <div className="composerStatsRow"><dt>
    {tone && <span className="composerStatsDot" data-token={tone} aria-hidden="true" />}
    {label}
  </dt><dd>{value}</dd></div>;
}

function object(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : undefined;
}

function number(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : undefined;
}

function count(value: unknown): string {
  const parsed = number(value);
  return parsed === undefined ? "—" : exactCount.format(parsed);
}

function compactTokens(value: number): string {
  return value < 1024 ? exactCount.format(value) : `${tokenCount.format(value / 1024)}K`;
}

function duration(value: unknown): string {
  const ms = number(value);
  if (ms === undefined) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`;
  const seconds = Math.floor(ms / 1000);
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

function cost(value: unknown, known: unknown): string {
  const amount = number(value);
  return known === true && amount !== undefined
    ? `$${(amount / 1_000_000).toLocaleString("en-US", {minimumFractionDigits: 2, maximumFractionDigits: 6})}`
    : "Unknown";
}

function usageCost(usage?: UsageRollup): string {
  if (!usage) return "Unknown";
  if (usage.cost_known) return cost(usage.cost_microunits, true);
  return (usage.priced_calls ?? 0) > 0 && (usage.unpriced_calls ?? 0) > 0
    ? `${cost(usage.cost_microunits, true)}+ (partly unpriced)` : "Unknown";
}
