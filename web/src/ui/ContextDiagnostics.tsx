import {useState} from "react";

// The runtime owns selection and coverage. This view renders its receipts;
// it never infers missing context or successful recovery from chat text.
export function ContextDiagnostics({receipt}: {receipt?: Readonly<Record<string, unknown>>}) {
  const [open, setOpen] = useState(false);
  const projection = record(receipt?.context_projection);
  const sample = record(receipt?.context_sample);
  const recovery = record(receipt?.context_recovery);
  const maintenance = Array.isArray(receipt?.context_maintenance) ? receipt.context_maintenance : [];
  if (!projection && maintenance.length === 0) return null;
  const omissions = Array.isArray(projection?.omissions) ? projection.omissions : [];
  const reasons = new Map<string, number>();
  for (const omission of omissions) {
    const reason = record(omission)?.reason;
    if (typeof reason === "string") reasons.set(reason, (reasons.get(reason) ?? 0) + 1);
  }
  return <section className="contextDiagnostics" aria-label="Context continuity">
    <p className="composerStatsNote">{projection
      ? projection.recovery_only === true
        ? "Source definitions are being recovered before work continues."
        : "Current task context retained. Earlier omitted content can be read when needed."
      : "Background context maintenance recorded."}</p>
    <button type="button" aria-expanded={open} onClick={() => setOpen(!open)}>
      Context selection details
    </button>
    {open && <div>
      {projection && <dl>
        <Row label="Input / output reserve" value={`${value(projection.input_tokens)} / ${value(projection.output_reserve)} tokens`} />
        <Row label="Raw history / ceiling" value={`${value(projection.raw_tokens)} / ${projection.raw_token_limited ? value(projection.raw_token_limit) : "No additional limit"}`} />
        <Row label="Calibration" value={sample?.window_observed ? "Provider usage baseline + estimated pending input" : "Token estimator"} />
        {sample?.compaction_headroom_tokens !== undefined && <>
          <Row label="Compaction headroom" value={`${value(sample.compaction_headroom_tokens)} tokens`} />
          <Row label="Compaction input target" value={`${value(sample.compaction_target_tokens ?? 0)} tokens (soft)`} />
        </>}
        <Row label="Recovery calls / failed" value={`${value(recovery?.calls)} / ${value(recovery?.failed)}`} />
        <Row label="Recovered bytes" value={value(recovery?.bytes)} />
        {[...reasons].map(([reason, count]) => <Row key={reason} label={reason} value={`${count} messages omitted`} />)}
      </dl>}
      <p className="composerStatsNote">Ranges are UTF-8 byte offsets. Coverage verifies source ranges; it does not certify summary accuracy. Recovery counts describe tool calls, not whether each call was necessary.</p>
      {projection && <details><summary>Source ranges and budget evidence</summary>
        <pre>{JSON.stringify({projection, sample}, null, 2)}</pre>
      </details>}
      {maintenance.length > 0 && <details><summary>Background summaries</summary>
        <p className="composerStatsNote">Maintenance costs are already included in session usage. Foreground wait counts time waiting for optional summary generation.</p>
        <pre>{JSON.stringify(maintenance, null, 2)}</pre>
      </details>}
    </div>}
  </section>;
}

function Row({label, value}: {label: string; value: string}) {
  return <div className="composerStatsRow"><dt>{label}</dt><dd>{value}</dd></div>;
}
function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}
function value(value: unknown): string {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value.toLocaleString("en-US") : "Not recorded";
}
