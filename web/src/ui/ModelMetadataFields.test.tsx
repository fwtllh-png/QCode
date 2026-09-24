import {cleanup, fireEvent, render, screen} from "@testing-library/react";
import {afterEach, describe, expect, it, vi} from "vitest";

import {
  emptyModelMetadataDraft,
  ModelMetadataFields,
  modelMetadataDraft,
  modelMetadataFromProbe,
  modelMetadataProblem,
  setupModelMetadata
} from "./ModelMetadataFields";

afterEach(cleanup);

function validDraft() {
  const draft = emptyModelMetadataDraft();
  draft.canonicalID = "vendor/model";
  draft.wireID = "model-1";
  draft.contextKTokens = "64";
  draft.maxOutputKTokens = "8";
  draft.capabilities.streaming = true;
  draft.capabilities.tool_calls = true;
  return draft;
}

describe("modelMetadataProblem", () => {
  it("preserves reasoning effort metadata returned by probing", () => {
    const draft = modelMetadataFromProbe("reasoner", {
      capabilities: {
        streaming: true,
        reasoning: true,
        reasoning_efforts: ["low", "medium", "high"],
        default_reasoning_effort: "medium",
        tool_calls: true,
        native_search: false,
        vision: false,
        image_input: false,
        prompt_cache: false
      }
    });
    expect(draft.reasoningEfforts).toBe("low, medium, high");
    expect(draft.defaultReasoningEffort).toBe("medium");
  });

  it("uses detected limits without exposing editable token fields", () => {
    const draft = modelMetadataFromProbe("reasoner", {
      models: [{
        id: "reasoner",
        context_tokens: 1_048_576,
        max_output_tokens: 393_216
      }],
      capabilities: {
        streaming: true,
        reasoning: false,
        tool_calls: true,
        native_search: false,
        vision: false,
        image_input: false,
        prompt_cache: false
      }
    });
    render(
      <ModelMetadataFields
        value={draft}
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.getByLabelText("Detected model limits").textContent)
      .toContain("Context 1,048,576");
    expect(screen.getByLabelText("Detected model limits").textContent)
      .toContain("Max output 393,216");
    expect(screen.queryByLabelText("Context tokens (K)")).toBeNull();
    expect(screen.queryByLabelText("Max output tokens (K)")).toBeNull();
  });

  it("keeps token inputs only when discovery omits limits", () => {
    const draft = modelMetadataFromProbe("reasoner", {
      capabilities: {
        streaming: true,
        reasoning: false,
        tool_calls: true,
        native_search: false,
        vision: false,
        image_input: false,
        prompt_cache: false
      }
    });
    render(
      <ModelMetadataFields
        value={draft}
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.getByLabelText("Context tokens (K)")).toBeTruthy();
    expect(screen.getByLabelText("Max output tokens (K)")).toBeTruthy();
  });

  it("edits limits in K and submits token counts", () => {
    const draft = validDraft();
    const onChange = vi.fn();
    render(<ModelMetadataFields value={draft} disabled={false} onChange={onChange} />);

    const context = screen.getByLabelText("Context tokens (K)");
    const output = screen.getByLabelText("Max output tokens (K)");
    expect(context).toHaveProperty("value", "64");
    expect(output).toHaveProperty("value", "8");
    expect(screen.getByText("1 K = 1,024 tokens")).toBeTruthy();

    fireEvent.change(context, {target: {value: "128"}});
    expect(setupModelMetadata(onChange.mock.lastCall![0]).context_tokens).toBe(131072);
    fireEvent.change(output, {target: {value: "0.5"}});
    expect(setupModelMetadata(onChange.mock.lastCall![0]).max_output_tokens).toBe(512);
    fireEvent.change(context, {target: {value: ""}});
    expect(onChange.mock.lastCall![0].contextKTokens).toBe("");
    expect(modelMetadataProblem(onChange.mock.lastCall![0], "openai_chat")).toBe("Enter token limits.");
  });

  it.each([1, 65537, 200000, Number.MAX_SAFE_INTEGER])(
    "preserves %i tokens when reopening saved limits in K",
    (tokens) => {
      const metadata = {
        ...setupModelMetadata(validDraft()),
        context_tokens: tokens,
        max_output_tokens: 1
      };
      const draft = modelMetadataDraft(metadata);
      expect(Number(draft.contextKTokens)).toBe(tokens / 1024);
      expect(modelMetadataProblem(draft, "openai_chat")).toBe("");
      expect(setupModelMetadata(draft)).toEqual(metadata);
    }
  );

  it("converts partial discovery to editable K values without rounding", () => {
    const draft = modelMetadataFromProbe("model-1", {
      models: [{id: "model-1", context_tokens: 65537}],
      capabilities: {
        streaming: true, reasoning: false, tool_calls: true,
        native_search: false, vision: false, image_input: false, prompt_cache: false
      }
    });
    expect(draft.limitsDetected).toBe(false);
    expect(draft.contextKTokens).toBe("64.0009765625");
    expect(draft.maxOutputKTokens).toBe("");
    draft.maxOutputKTokens = "0.5";
    expect(modelMetadataProblem(draft, "openai_chat")).toBe("");
    expect(setupModelMetadata(draft)).toMatchObject({
      context_tokens: 65537, max_output_tokens: 512
    });
  });

  it.each(["", "0", "-1", "0.1", "Infinity", "NaN", "8796093022208"])(
    "rejects invalid token counts from K input %j",
    (value) => {
      for (const key of ["contextKTokens", "maxOutputKTokens"] as const) {
        const draft = {...validDraft(), [key]: value};
        expect(modelMetadataProblem(draft, "openai_chat")).toBe("Enter token limits.");
      }
    }
  );

  it("shows explicit effort metadata for reasoning models", () => {
    const draft = validDraft();
    draft.capabilities.reasoning = true;
    render(
      <ModelMetadataFields
        value={draft}
        disabled={false}
        onChange={vi.fn()}
      />
    );

    expect(screen.getByLabelText("Reasoning efforts")).toBeTruthy();
    expect(screen.getByLabelText("Default reasoning effort")).toBeTruthy();
  });

  it("accepts complete explicit metadata", () => {
    const draft = validDraft();
    draft.capabilities.reasoning = true;
    draft.reasoningEfforts = "off, high, max";
    draft.defaultReasoningEffort = "high";
    expect(modelMetadataProblem(draft, "openai_chat")).toBe("");
  });

  it("validates identity and reasoning declarations", () => {
    const draft = validDraft();
    draft.wireID = "model id";
    expect(modelMetadataProblem(draft, "openai_chat")).toContain(
      "invalid"
    );

    draft.wireID = "model-1";
    draft.capabilities.reasoning = true;
    draft.reasoningEfforts = "high, high";
    expect(modelMetadataProblem(draft, "openai_chat")).toContain("invalid");

    draft.reasoningEfforts = "high";
    draft.defaultReasoningEffort = "max";
    expect(modelMetadataProblem(draft, "openai_chat")).toContain(
      "invalid"
    );
  });

  it("validates capability dependencies and protocol", () => {
    const draft = validDraft();
    draft.capabilities.tool_calls = false;
    expect(modelMetadataProblem(draft, "openai_chat")).toBe(
      "QCode requires tool calling."
    );
    draft.capabilities.tool_calls = true;
    draft.capabilities.thinking_toggle = true;
    expect(modelMetadataProblem(draft, "openai_chat")).toContain(
      "invalid"
    );

    draft.capabilities.thinking_toggle = false;
    draft.capabilities.automatic_prompt_cache = true;
    expect(modelMetadataProblem(draft, "openai_chat")).toBe(
      "Enable prompt cache first."
    );

    draft.capabilities.automatic_prompt_cache = false;
    draft.capabilities.incremental_responses = true;
    expect(modelMetadataProblem(draft, "openai_chat")).toBe(
      "Incremental responses require Responses."
    );
    expect(modelMetadataProblem(draft, "openai_responses")).toBe("");
  });

  it("reports the invalid metadata field group", () => {
    const draft = emptyModelMetadataDraft("model-v1");
    expect(modelMetadataProblem(draft, "openai_chat")).toBe(
      "Enter token limits."
    );
    draft.contextKTokens = "1";
    draft.maxOutputKTokens = "2";
    expect(modelMetadataProblem(draft, "openai_chat")).toBe(
      "Output exceeds context."
    );
  });
});
