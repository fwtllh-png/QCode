import {describe, expect, it} from "vitest";
import {
  compactCatalogSelectWidth,
  compactSelectWidth
} from "./ConversationChrome";

describe("compact catalog select width", () => {
  it("sizes to the longest option so model labels stay readable", () => {
    expect(compactCatalogSelectWidth(["glm", "long-model-name-v2"]))
      .toEqual({width: "calc(18ch + 24px)", maxWidth: "none"});
  });

  it("keeps a readable minimum and bounded maximum", () => {
    expect(compactCatalogSelectWidth(["gpt"]))
      .toEqual({width: "calc(6ch + 24px)", maxWidth: "none"});
    expect(compactCatalogSelectWidth(["x".repeat(80)]))
      .toEqual({width: "calc(32ch + 24px)", maxWidth: "none"});
  });

  it("counts unicode labels by characters, not utf-16 units", () => {
    expect(compactCatalogSelectWidth(["模型", "deepseek-reasoner-pro"]).width)
      .toBe("calc(21ch + 24px)");
  });
});

describe("compact select width", () => {
  it("still follows the selected value with the historical cap", () => {
    expect(compactSelectWidth("auto"))
      .toEqual({width: "calc(4ch + 24px)"});
    expect(compactSelectWidth("x".repeat(40)))
      .toEqual({width: "calc(18ch + 24px)"});
  });
});
