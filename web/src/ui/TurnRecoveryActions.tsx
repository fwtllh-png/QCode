import {GitFork, Play, RefreshCw, RotateCcw} from "lucide-react";
import {useRef, useState} from "react";
import type {SessionCheckpoint} from "../protocol";
import type {ConversationNode} from "../projection/conversation";
import type {RuntimeClient} from "../runtime/client";
import {fullAccessDescription} from "./approvalPosture";
import {useWorkbenchSnapshot} from "./useRuntimeView";

export function TurnRecoveryActions({entry, checkpoint, client, onError}: {
  entry: Extract<ConversationNode, {kind: "status"}>;
  checkpoint?: SessionCheckpoint;
  client: RuntimeClient;
  onError: (error: unknown) => void;
}) {
  const snapshot = useWorkbenchSnapshot(client);
  const [pending, setPending] = useState(false);
  const pendingRef = useRef(false);
  const profile = snapshot.profile;
  const permissionRequired = entry.recovery?.action === "change_approval_posture" &&
    profile?.profile.approval_posture === "never";
  const canChangePermission = profile?.capabilities.mutable_fields.includes("approval_posture");
  const disabled = pending || Boolean(snapshot.hydratingSessionID) ||
    Boolean(snapshot.conversation.activeTurnID) ||
    Boolean(snapshot.profileUpdatingSessionIDs?.includes(snapshot.selectedSessionID));

  const run = async (action: () => Promise<unknown>) => {
    if (disabled || pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    try {
      await action();
    } catch (error) {
      onError(error);
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  const changePermissionAndContinue = (posture: "auto" | "bypass") => run(async () => {
    const sessionID = snapshot.selectedSessionID;
    const workspaceID = snapshot.selectedWorkspaceID;
    const result = await client.updateProfile({approval_posture: posture});
    const current = client.getSnapshot();
    if (current.selectedSessionID !== sessionID || current.selectedWorkspaceID !== workspaceID) {
      throw new Error("Settings were saved. Return to the original session to continue.");
    }
    if (result.profile.approval_posture !== posture ||
        current.profile?.profile.approval_posture !== posture) {
      throw new Error("The selected permission mode is not active. Check the session settings before continuing.");
    }
    await client.recoverTurn(entry.turnID, "continue");
  });

  return (
    <div className="turnRecovery">
      <span className="turnRecoveryStatus">
        {permissionRequired
          ? canChangePermission
            ? `Auto asks when needed. ${fullAccessDescription}`
            : "The runtime keeps this session read only. Change its permission setting before continuing."
          : recoverySummary(entry.recovery?.sideEffects ?? "unknown")}
      </span>
      <div className="artifactActions">
        {permissionRequired && canChangePermission && entry.recovery?.canContinue ? (
          <>
            <button disabled={disabled} onClick={() => void changePermissionAndContinue("auto")}>
              Use Auto and continue
            </button>
            <button disabled={disabled} onClick={() => void changePermissionAndContinue("bypass")}>
              Use Full Access and continue
            </button>
          </>
        ) : (
          <>
            {entry.recovery?.canRetry && (
              <button disabled={disabled || permissionRequired}
                onClick={() => void run(() => client.recoverTurn(entry.turnID, "retry"))}>
                <RotateCcw size={13} /> Retry
              </button>
            )}
            {entry.recovery?.canContinue && (
              <button disabled={disabled || permissionRequired}
                onClick={() => void run(() => client.recoverTurn(entry.turnID, "continue"))}>
                <Play size={13} /> Continue
              </button>
            )}
          </>
        )}
        {checkpoint?.can_restore && (
          <button disabled={disabled}
            onClick={() => void run(() => client.restoreCheckpoint(checkpoint.id))}>
            <RefreshCw size={13} /> Restore
          </button>
        )}
        {checkpoint?.can_fork && (
          <button disabled={disabled}
            onClick={() => void run(() => client.forkCheckpoint(checkpoint.id))}>
            <GitFork size={13} /> Fork
          </button>
        )}
      </div>
    </div>
  );
}

function recoverySummary(sideEffects: string): string {
  switch (sideEffects) {
    case "draft": return "Draft saved. Review the recovery guidance before continuing.";
    case "committed": return "Workspace changes were kept.";
    case "rolled_back": return "Workspace changes were rolled back.";
    case "none": return "No workspace changes were made.";
    default: return "Review the workspace before continuing.";
  }
}
