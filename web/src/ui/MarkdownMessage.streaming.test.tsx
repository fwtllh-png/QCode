import {cleanup, render, screen, waitFor} from "@testing-library/react";
import {afterEach, describe, expect, it, vi} from "vitest";
import {MarkdownMessage} from "./MarkdownMessage";

const parsing = vi.hoisted(() => ({calls: 0}));
vi.mock("remark-gfm", async (importOriginal) => {
  const actual = await importOriginal<typeof import("remark-gfm")>();
  return {default: function (this: unknown, ...args: unknown[]) {
    parsing.calls += 1;
    return Reflect.apply(actual.default, this, args);
  }};
});
const highlighting = vi.hoisted(() => ({calls: [] as Array<{code: string; language: string}>}));
vi.mock("./codeHighlight", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./codeHighlight")>();
  return {highlightCode(code: string, language: string): string {
    highlighting.calls.push({code, language});
    return actual.highlightCode(code, language);
  }};
});
afterEach(() => {
  cleanup();
  parsing.calls = 0;
  highlighting.calls.length = 0;
});

describe("streaming Markdown", () => {
  it("does not reparse completed messages when a sibling streams", async () => {
    const history = "**Historical answer**\n\n```go\nfunc main() {}\n```";
    const view = render(<>
      <MarkdownMessage text={history} settled />
      <MarkdownMessage text="Live" settled={false} />
    </>);
    const before = parsing.calls;
    view.rerender(<>
      <MarkdownMessage text={history} settled />
      <MarkdownMessage text="Live **update**" settled={false} />
    </>);
    await screen.findByText("update");
    expect(parsing.calls - before).toBe(1);
    expect(screen.getByText("Historical answer").tagName).toBe("STRONG");
  });

  it("applies authoritative final output without leaving a deferred draft behind", async () => {
    const view = render(<MarkdownMessage text="Provisional draft" settled={false} />);
    view.rerender(<MarkdownMessage text="Provisional draft continues" settled={false} />);
    view.rerender(<MarkdownMessage text="**Final answer**" settled />);
    expect(screen.queryByText(/Provisional draft/)).toBeNull();
    expect(screen.getByText("Final answer").tagName).toBe("STRONG");
    await waitFor(() => expect(view.container.textContent).toBe("Final answer"));
  });

  it("preserves Markdown syntax that resolves across stream chunks", async () => {
    const view = render(<MarkdownMessage text="[Source][ref]\n\n```typescript\nconst value" settled={false} />);
    view.rerender(<MarkdownMessage
      text={'[Source][ref]\n\n```typescript\nconst value = 1;\n```\n\n[ref]: https://example.test/source'}
      settled={false}
    />);
    expect(await screen.findByRole("link", {name: "Source"})).toBeTruthy();
    expect(screen.getByRole("region", {name: "typescript code"}).textContent)
      .toContain("const value = 1;");
  });

  it("does not re-highlight a completed code block while a later chunk streams", async () => {
    const head = "```go\nfunc main() {}\n```\n\nAnswer ";
    const view = render(<MarkdownMessage text={`${head}one`} settled={false} />);
    expect(await screen.findByRole("region", {name: "go code"})).toBeTruthy();
    highlighting.calls.length = 0;
    view.rerender(<MarkdownMessage text={`${head}two`} settled={false} />);
    view.rerender(<MarkdownMessage text={`${head}three`} settled={false} />);
    expect(screen.getByRole("region", {name: "go code"}).textContent)
      .toContain("func main() {}");
    expect(highlighting.calls).toEqual([]);
  });

  it("re-highlights the streaming code block as its content grows", async () => {
    // JSX 双引号属性不处理 \n 转义，必须用花括号内的 JS 字符串字面量。
    const view = render(<MarkdownMessage text={"```typescript\nconst value"} settled={false} />);
    highlighting.calls.length = 0;
    view.rerender(<MarkdownMessage text={"```typescript\nconst value = 1;"} settled={false} />);
    const region = await screen.findByRole("region", {name: "typescript code"});
    expect(region.textContent).toContain("const value = 1;");
    expect(highlighting.calls.some((call) => call.code === "const value = 1;")).toBe(true);
  });
});
