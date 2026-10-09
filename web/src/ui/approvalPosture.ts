export function normalizeApprovalPosture(value: string): string {
  return value === "suggest" ? "auto" : value;
}

export function approvalPostureLabel(value: string): string {
  switch (normalizeApprovalPosture(value)) {
    case "auto": return "Auto";
    case "never": return "Read only";
    case "bypass": return "Full Access";
    default: return value;
  }
}

export const fullAccessDescription =
  "Full Access allows file changes, network access and Git push without tool approval prompts. Commands may explicitly run outside QCode's sandbox. Explicit approval and deny rules still apply.";

export const approvalPostureDescription =
  `Auto runs routine actions and asks before commands run outside the sandbox. ${fullAccessDescription} Read only prevents changes and host commands.`;

export const approvalPostures = ["never", "auto", "bypass"];
