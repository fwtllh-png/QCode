import {useId} from "react";
import type {
  SetupModelCapabilities,
  SetupModelMetadata,
  SetupProbeResult
} from "../protocol";
import {Switch} from "./primitives/Switch";
import "./ModelMetadataFields.css";

// UI 单位约定：1 K = 1024 tokens；协议与持久化始终使用 tokens。
const tokensPerK = 1024;

export interface ModelMetadataDraft {
  canonicalID: string;
  wireID: string;
  contextKTokens: string;
  maxOutputKTokens: string;
  limitsDetected: boolean;
  capabilities: SetupModelCapabilities;
  reasoningEfforts: string;
  defaultReasoningEffort: string;
}

export function emptyModelMetadataDraft(modelID = ""): ModelMetadataDraft {
  return {
    canonicalID: modelID,
    wireID: modelID,
    contextKTokens: "",
    maxOutputKTokens: "",
    limitsDetected: false,
    capabilities: {
      streaming: true,
      reasoning: false,
      tool_calls: false,
      native_search: false,
      incremental_responses: false,
      vision: false,
      image_input: false,
      prompt_cache: false,
      automatic_prompt_cache: false,
      thinking_toggle: false
    },
    reasoningEfforts: "",
    defaultReasoningEffort: ""
  };
}

export function modelMetadataDraft(
  metadata?: SetupModelMetadata,
  modelID = ""
): ModelMetadataDraft {
  if (!metadata) return emptyModelMetadataDraft(modelID);
  return {
    canonicalID: metadata.canonical_id,
    wireID: metadata.wire_id,
    contextKTokens: String(metadata.context_tokens / tokensPerK),
    maxOutputKTokens: String(metadata.max_output_tokens / tokensPerK),
    limitsDetected: false,
    capabilities: {...metadata.capabilities, streaming: true},
    reasoningEfforts: metadata.capabilities.reasoning_efforts?.join(", ") ?? "",
    defaultReasoningEffort:
      metadata.capabilities.default_reasoning_effort ?? ""
  };
}

export function modelMetadataFromProbe(
  modelID: string,
  result: SetupProbeResult
): ModelMetadataDraft {
  const discovered = result.models?.find((model) => model.id === modelID);
  const limitsDetected = Boolean(
    discovered?.context_tokens && discovered.max_output_tokens
  );
  return {
    canonicalID: modelID,
    wireID: modelID,
    contextKTokens: discovered?.context_tokens
      ? String(discovered.context_tokens / tokensPerK)
      : "",
    maxOutputKTokens: discovered?.max_output_tokens
      ? String(discovered.max_output_tokens / tokensPerK)
      : "",
    limitsDetected,
    capabilities: {
      streaming: result.capabilities.streaming,
      reasoning: result.capabilities.reasoning,
      reasoning_efforts: result.capabilities.reasoning_efforts,
      default_reasoning_effort:
        result.capabilities.default_reasoning_effort,
      tool_calls: result.capabilities.tool_calls,
      native_search: false,
      incremental_responses: false,
      vision: false,
      image_input: false,
      prompt_cache: false,
      automatic_prompt_cache: false,
      thinking_toggle: false
    },
    reasoningEfforts: result.capabilities.reasoning_efforts?.join(", ") ?? "",
    defaultReasoningEffort:
      result.capabilities.default_reasoning_effort ?? ""
  };
}

function efforts(value: string): string[] {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

const modelIDPattern = /^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$/;
const validEfforts = "none off minimal low medium high xhigh max".split(" ");

export function modelMetadataProblem(
  draft: ModelMetadataDraft,
  protocol: string
): string {
  const contextTokens = Number(draft.contextKTokens) * tokensPerK;
  const maxOutputTokens = Number(draft.maxOutputKTokens) * tokensPerK;
  const capabilities = draft.capabilities;
  const declaredEfforts = efforts(draft.reasoningEfforts);
  const defaultEffort = draft.defaultReasoningEffort.trim();
  if (!modelIDPattern.test(draft.canonicalID.trim()) ||
      !modelIDPattern.test(draft.wireID.trim())) {
    return "Model IDs are invalid.";
  }
  if (!Number.isSafeInteger(contextTokens) || contextTokens <= 0 ||
      !Number.isSafeInteger(maxOutputTokens) || maxOutputTokens <= 0) {
    return "Enter token limits.";
  }
  if (maxOutputTokens > contextTokens) {
    return "Output exceeds context.";
  }
  if (!capabilities.tool_calls) {
    return "QCode requires tool calling.";
  }
  if (!capabilities.reasoning &&
      (declaredEfforts.length > 0 || defaultEffort ||
       capabilities.thinking_toggle) ||
      declaredEfforts.some((effort) => !validEfforts.includes(effort)) ||
      new Set(declaredEfforts).size !== declaredEfforts.length ||
      Boolean(defaultEffort && !declaredEfforts.includes(defaultEffort))) {
    return "Reasoning settings are invalid.";
  }
  if (capabilities.automatic_prompt_cache && !capabilities.prompt_cache) {
    return "Enable prompt cache first.";
  }
  if (capabilities.incremental_responses &&
      protocol !== "openai_responses") {
    return "Incremental responses require Responses.";
  }
  return "";
}

export function setupModelMetadata(
  draft: ModelMetadataDraft
): SetupModelMetadata {
  return {
    canonical_id: draft.canonicalID.trim(),
    wire_id: draft.wireID.trim(),
    context_tokens: Number(draft.contextKTokens) * tokensPerK,
    max_output_tokens: Number(draft.maxOutputKTokens) * tokensPerK,
    capabilities: {
      ...draft.capabilities,
      reasoning_efforts: draft.capabilities.reasoning
        ? efforts(draft.reasoningEfforts)
        : undefined,
      default_reasoning_effort: draft.capabilities.reasoning
        ? draft.defaultReasoningEffort.trim() || undefined
        : undefined
    }
  };
}

type TextFieldKey =
  | "canonicalID"
  | "wireID"
  | "contextKTokens"
  | "maxOutputKTokens"
  | "reasoningEfforts"
  | "defaultReasoningEffort";

interface TextField {
  key: TextFieldKey;
  label: string;
  type?: "number";
  placeholder?: string;
}

const limitFields: TextField[] = [
  {key: "contextKTokens", label: "Context tokens (K)", type: "number"},
  {key: "maxOutputKTokens", label: "Max output tokens (K)", type: "number"}
];

const reasoningFields: TextField[] = [
  {
    key: "reasoningEfforts",
    label: "Reasoning efforts",
    placeholder: "none, low, medium, high, xhigh"
  },
  {
    key: "defaultReasoningEffort",
    label: "Default reasoning effort",
    placeholder: "high"
  }
];

export function ModelMetadataFields({
  value,
  disabled,
  onChange
}: {
  value: ModelMetadataDraft;
  disabled: boolean;
  onChange: (value: ModelMetadataDraft) => void;
}) {
  const tokenUnitID = useId();
  const updateText = (key: TextFieldKey, text: string) => {
    onChange({...value, [key]: text});
  };
  return (
    <div className="modelMetadataFields">
      <div className="modelMetadataCapabilities" role="group" aria-label="Model capabilities">
        <Switch
          label="Tool calling"
          checked={value.capabilities.tool_calls}
          disabled={disabled}
          onChange={(checked) => onChange({
            ...value, capabilities: {...value.capabilities, tool_calls: checked}
          })}
        >
          <span>Tool calling</span>
        </Switch>
        <Switch
          label="Reasoning"
          checked={value.capabilities.reasoning}
          disabled={disabled}
          onChange={(checked) => onChange({
            ...value,
            capabilities: {...value.capabilities, reasoning: checked, thinking_toggle: false},
            reasoningEfforts: "", defaultReasoningEffort: ""
          })}
        >
          <span>Reasoning</span>
        </Switch>
      </div>
      <div className="modelMetadataGrid">
        {value.limitsDetected ? (
          <div className="detectedModelLimits" aria-label="Detected model limits">
            <span>Context {(Number(value.contextKTokens) * tokensPerK).toLocaleString()} tokens</span>
            <span>Max output {(Number(value.maxOutputKTokens) * tokensPerK).toLocaleString()} tokens</span>
          </div>
        ) : limitFields.map(({key, label, type, placeholder}) => (
          <label className="modelMetadataField" key={key}>
            <span>{label}</span>
            <input
              type={type}
              min={1 / tokensPerK}
              step="any"
              aria-label={label}
              aria-describedby={tokenUnitID}
              placeholder={placeholder}
              value={value[key]}
              disabled={disabled}
              onChange={(event) => updateText(key, event.target.value)}
            />
          </label>
        ))}
        {!value.limitsDetected && (
          <small className="modelMetadataUnit" id={tokenUnitID}>
            1 K = 1,024 tokens
          </small>
        )}
        {value.capabilities.reasoning && reasoningFields.map(
          ({key, label, type, placeholder}) => (
            <label className="modelMetadataField" key={key}>
              <span>{label}</span>
              <input
                type={type}
                min="1"
                step="1"
                aria-label={label}
                placeholder={placeholder}
                value={value[key]}
                disabled={disabled}
                onChange={(event) => updateText(key, event.target.value)}
              />
            </label>
          )
        )}
      </div>
    </div>
  );
}
