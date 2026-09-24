import {useMemo, useRef, useState} from "react";
import type {RuntimeEvent, TraceSnapshot} from "../protocol";
import {projectTrajectory} from "../projection/trajectory";
import {RecordInspector} from "./Trajectory";
import {useModalFocus} from "./primitives/useModalFocus";

// 会话内工具检查面板：复用 Trajectory 的 RecordInspector，以右侧滑入的
// 模态面板呈现，避免“点一下 Inspect 整页切走”的导航跳变。
// 主视图的 Trajectory 标签页保持不变，两条入口并存。
export function InspectPanel({
  events,
  trace,
  callID,
  onClose,
  onOpenChat
}: {
  events: readonly RuntimeEvent[];
  trace?: TraceSnapshot;
  callID: string;
  onClose: () => void;
  onOpenChat: (turnID: string, callID?: string, entryID?: string) => void;
}) {
  const projection = useMemo(
    () => projectTrajectory(events, trace),
    [events, trace]
  );
  const records = projection.records;
  const dialogRef = useRef<HTMLElement>(null);
  useModalFocus(dialogRef, true, onClose);
  const [selectedID, setSelectedID] = useState(() =>
    records.find((record) => record.callID === callID)?.id ??
    records[records.length - 1]?.id ?? ""
  );
  const record = records.find((candidate) => candidate.id === selectedID) ??
    records[records.length - 1];
  const span = projection.spans.find(
    (candidate) => candidate.recordID === record?.id
  );
  if (!record) return null;
  return (
    <div
      className="inspectPanelOverlay"
      data-motion-backdrop
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section
        ref={dialogRef}
        className="inspectPanelSurface"
        data-motion-surface
        role="dialog"
        aria-modal="true"
        aria-label="Inspect tool call"
      >
        <RecordInspector
          record={record}
          span={span}
          records={records}
          onClose={onClose}
          onOpenChat={() => {
            const turnID = record.turnID;
            onClose();
            onOpenChat(turnID, record.callID || undefined, record.id);
          }}
          onSelect={setSelectedID}
        />
      </section>
    </div>
  );
}
