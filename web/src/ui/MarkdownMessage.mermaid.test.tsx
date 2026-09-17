import {cleanup, fireEvent, render, screen, waitFor} from "@testing-library/react";
import {afterEach, beforeEach, describe, expect, it, vi} from "vitest";
import {MarkdownMessage} from "./MarkdownMessage";

const mermaid = vi.hoisted(() => ({
  initialize: vi.fn(),
  render: vi.fn()
}));

vi.mock("mermaid", () => ({
  default: mermaid
}));

const clipboardWrite = vi.fn(async () => {});

Object.defineProperty(navigator, "clipboard", {
  configurable: true,
  value: {writeText: clipboardWrite}
});

const source = "graph TD\nA-->B";

function mermaidText(body: string): string {
  return ["```mermaid", body, "```"].join("\n");
}

beforeEach(() => {
  delete document.documentElement.dataset.theme;
});

afterEach(() => {
  cleanup();
  clipboardWrite.mockClear();
  mermaid.initialize.mockClear();
  mermaid.render.mockReset();
  delete document.documentElement.dataset.theme;
});

describe("MarkdownMessage mermaid diagrams", () => {
  it("renders a settled mermaid block as a diagram and copies its source", async () => {
    mermaid.render.mockResolvedValue({
      svg: "<svg><style>.node rect { fill: #ececff; }</style>" +
        '<rect style="color: rgb(11, 22, 33)" width="10" height="10"></rect></svg>'
    });
    render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled
      />
    );

    const region = await screen.findByRole("region", {name: "Mermaid diagram"});
    await waitFor(() => {
      // jsdom cannot observe the CSP-safe style re-application (its connected
      // <style> handling rewrites inline attributes); browser-level checks
      // against the strict CSP cover that path.
      expect(region.querySelector(".markdownDiagramSvg svg")).toBeTruthy();
      expect(region.querySelector("rect")).toBeTruthy();
    });
    expect(mermaid.initialize).toHaveBeenCalledWith(expect.objectContaining({
      securityLevel: "strict",
      theme: "default"
    }));
    expect(mermaid.render).toHaveBeenCalledWith(
      expect.stringMatching(/^mermaid-diagram-/),
      source
    );

    fireEvent.click(screen.getByRole("button", {name: "Copy code"}));
    await waitFor(() => {
      expect(clipboardWrite).toHaveBeenCalledWith(source);
    });
  });

  it("falls back to the source code when the diagram fails to render", async () => {
    mermaid.render.mockRejectedValue(new Error("unknown diagram type"));
    render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled
      />
    );

    expect((await screen.findByRole("alert")).textContent).toBe("Diagram unavailable");
    expect(screen.getByRole("region", {name: "Mermaid diagram"})
      .querySelector("pre")?.textContent).toBe(`${source}\n`);
    expect(screen.queryByText("Rendering diagram")).toBeNull();
  });

  it("keeps streaming mermaid blocks as plain code", () => {
    render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled={false}
      />
    );

    expect(screen.getByRole("region", {name: "mermaid code"})
      .querySelector("pre")?.textContent).toBe(`${source}\n`);
    expect(mermaid.render).not.toHaveBeenCalled();
  });

  it("re-renders the diagram when the application theme changes", async () => {
    mermaid.render.mockResolvedValue({svg: "<svg><g>diagram-body</g></svg>"});
    render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled
      />
    );
    await screen.findByRole("region", {name: "Mermaid diagram"});

    document.documentElement.dataset.theme = "dark";
    await waitFor(() => {
      expect(mermaid.initialize).toHaveBeenCalledWith(expect.objectContaining({
        theme: "dark"
      }));
    });
  });

  it("zooms the diagram in steps and resets to 100%", async () => {
    const measure = vi
      .spyOn(SVGElement.prototype, "getBoundingClientRect")
      .mockReturnValue({
        width: 800,
        height: 400,
        x: 0,
        y: 0,
        top: 0,
        left: 0,
        right: 800,
        bottom: 400,
        toJSON: () => ({})
      } as DOMRect);
    mermaid.render.mockResolvedValue({svg: '<svg viewBox="0 0 800 400"></svg>'});
    const {container} = render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled
      />
    );

    const reset = await screen.findByRole("button", {name: "Reset diagram zoom"});
    expect(reset.textContent).toBe("100%");
    await waitFor(() => {
      expect(
        container.querySelector<HTMLElement>(".markdownDiagramZoom")?.style.transform
      ).toBe("scale(1)");
    });

    fireEvent.click(screen.getByRole("button", {name: "Zoom in diagram"}));
    await waitFor(() => {
      expect(reset.textContent).toBe("125%");
      expect(
        container.querySelector<HTMLElement>(".markdownDiagramZoom")?.style.transform
      ).toBe("scale(1.25)");
      expect(container.querySelector<HTMLElement>(".markdownDiagramStage")?.style.width)
        .toBe("1000px");
    });

    fireEvent.click(screen.getByRole("button", {name: "Zoom out diagram"}));
    fireEvent.click(screen.getByRole("button", {name: "Zoom out diagram"}));
    await waitFor(() => expect(reset.textContent).toBe("80%"));

    fireEvent.click(reset);
    await waitFor(() => expect(reset.textContent).toBe("100%"));
    measure.mockRestore();
  });

  it("opens the diagram full screen and closes it on escape", async () => {
    mermaid.render.mockResolvedValue({svg: "<svg><rect></rect></svg>"});
    render(
      <MarkdownMessage
        text={mermaidText(source)}
        settled
      />
    );

    fireEvent.click(await screen.findByRole("button", {
      name: "Open diagram full screen"
    }));
    const dialog = await screen.findByRole("dialog", {name: "Mermaid diagram"});
    expect(dialog.className).toContain("markdownDiagramExpanded");
    expect(document.documentElement.style.overflow).toBe("hidden");
    expect(screen.getByRole("button", {name: "Close full screen diagram"}))
      .toBeTruthy();

    fireEvent.keyDown(document, {key: "Escape"});
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(screen.getByRole("button", {name: "Open diagram full screen"}))
        .toBeTruthy();
    });
    expect(document.documentElement.style.overflow).toBe("");
  });
});
