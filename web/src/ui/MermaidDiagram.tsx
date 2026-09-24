import {LoaderCircle, Maximize2, X, ZoomIn, ZoomOut} from "lucide-react";
import {
  useEffect,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode
} from "react";
import {createPortal} from "react-dom";

let renderCount = 0;
let renderQueue: Promise<void> = Promise.resolve();

// 图表测量尺寸缓存，按“主题+源码”为键：重挂载时先按上次尺寸占位。
const diagramBaseSizes = new Map<string, {width: number; height: number}>();

function cacheKey(source: string, theme: string): string {
  return `${theme}\u0000${source}`;
}

// Viewer interaction range: 0.25x–4x in 1.25x steps, mirroring common
// document-zoom controls.
const SCALE_STEP = 1.25;
const SCALE_MIN = 0.25;
const SCALE_MAX = 4;

function diagramTheme(): "dark" | "default" {
  return document.documentElement.dataset.theme === "dark" ? "dark" : "default";
}

function clampScale(value: number): number {
  return Math.min(SCALE_MAX, Math.max(SCALE_MIN, value));
}

export function MermaidDiagram({
  source,
  fallback
}: {
  source: string;
  fallback: ReactNode;
}) {
  const [theme, setTheme] = useState(diagramTheme);
  const [diagram, setDiagram] = useState<{
    source: string;
    theme: string;
    svg: string;
  } | null>(null);
  const [failed, setFailed] = useState(false);
  const [scale, setScale] = useState(1);
  const [expanded, setExpanded] = useState(false);
  // 同源图表的测量尺寸缓存：重渲染/重挂载（流式 settle 换装、主题切换）
  // 时按上次尺寸预留高度，避免图表换装把后续内容推开。
  const [base, setBase] = useState<{width: number; height: number} | null>(
    () => (source
      ? diagramBaseSizes.get(cacheKey(source, diagramTheme())) ?? null
      : null)
  );
  const container = useRef<HTMLDivElement | null>(null);
  const scrollFrame = useRef<HTMLDivElement | null>(null);
  const toggle = useRef<HTMLButtonElement | null>(null);
  const pan = useRef<{
    pointerId: number;
    x: number;
    y: number;
    left: number;
    top: number;
  } | null>(null);

  useEffect(() => {
    const observer = new MutationObserver(() => setTheme(diagramTheme()));
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"]
    });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    let cancelled = false;
    setFailed(false);
    renderQueue = renderQueue.then(async () => {
      if (cancelled) return;
      const mermaid = (await import("mermaid")).default;
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme,
        fontFamily: "inherit"
      });
      const {svg} = await mermaid.render(`mermaid-diagram-${++renderCount}`, source);
      if (!cancelled) setDiagram({source, theme, svg});
    }).catch(() => {
      if (!cancelled) setFailed(true);
    });
    return () => {
      cancelled = true;
    };
  }, [source, theme]);

  useEffect(() => {
    const node = container.current;
    if (!node) return;
    const sheet = applyDiagramStyles(node);
    const svg = node.querySelector("svg");
    // The diagram must follow the stage width at every zoom level; mermaid's
    // inline max-width would cap it at the natural 100% size.
    if (svg) svg.style.maxWidth = "none";
    const rect = svg?.getBoundingClientRect();
    const measured = rect && rect.width > 0 && rect.height > 0
      ? {width: rect.width, height: rect.height}
      : null;
    setBase(measured);
    if (measured && diagram) {
      diagramBaseSizes.set(cacheKey(diagram.source, diagram.theme), measured);
    }
    setScale(1);
    return () => {
      if (sheet) detachDiagramSheet(sheet);
    };
  }, [diagram]);

  useEffect(() => {
    const node = scrollFrame.current;
    if (!node) return;
    const onWheel = (event: WheelEvent) => {
      if (!event.ctrlKey && !event.metaKey) return;
      event.preventDefault();
      setScale((value) => clampScale(
        event.deltaY < 0 ? value * SCALE_STEP : value / SCALE_STEP
      ));
    };
    node.addEventListener("wheel", onWheel, {passive: false});
    return () => node.removeEventListener("wheel", onWheel);
  }, []);

  useEffect(() => {
    if (!expanded) return;
    const previousActive = document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null;
    const root = document.documentElement;
    const previousOverflow = root.style.overflow;
    root.style.overflow = "hidden";
    toggle.current?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !event.defaultPrevented) setExpanded(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => {
      root.style.overflow = previousOverflow;
      document.removeEventListener("keydown", onKeyDown);
      previousActive?.focus();
    };
  }, [expanded]);

  const zoom = (direction: 1 | -1) => {
    setScale((value) => clampScale(
      direction > 0 ? value * SCALE_STEP : value / SCALE_STEP
    ));
  };

  const startPan = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0) return;
    const node = scrollFrame.current;
    if (!node) return;
    if (node.scrollWidth <= node.clientWidth && node.scrollHeight <= node.clientHeight) {
      return;
    }
    pan.current = {
      pointerId: event.pointerId,
      x: event.clientX,
      y: event.clientY,
      left: node.scrollLeft,
      top: node.scrollTop
    };
    node.setPointerCapture(event.pointerId);
  };

  const movePan = (event: ReactPointerEvent<HTMLDivElement>) => {
    const state = pan.current;
    const node = scrollFrame.current;
    if (!state || !node || event.pointerId !== state.pointerId) return;
    node.scrollLeft = state.left - (event.clientX - state.x);
    node.scrollTop = state.top - (event.clientY - state.y);
  };

  const endPan = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (pan.current?.pointerId !== event.pointerId) return;
    pan.current = null;
    scrollFrame.current?.releasePointerCapture(event.pointerId);
  };

  if (failed) {
    return (
      <div className="markdownDiagramError">
        <span
          className="markdownDiagramNotice"
          role="alert"
        >
          Diagram unavailable
        </span>
        {fallback}
      </div>
    );
  }
  const current = diagram?.source === source && diagram?.theme === theme
    ? diagram
    : null;
  const shell = (
    <div
      className={expanded
        ? "markdownDiagramShell markdownDiagramExpanded"
        : "markdownDiagramShell"}
      data-state={current ? "ready" : "loading"}
      role={expanded ? "dialog" : undefined}
      aria-modal={expanded || undefined}
      aria-label={expanded ? "Mermaid diagram" : undefined}
    >
      {current && (
        <div className="markdownDiagramToolbar">
          <button
            type="button"
            aria-label="Zoom out diagram"
            title="Zoom out"
            onClick={() => zoom(-1)}
          >
            <ZoomOut size={13} />
          </button>
          <button
            type="button"
            className="markdownDiagramScale"
            aria-label="Reset diagram zoom"
            title="Reset zoom"
            onClick={() => setScale(1)}
          >
            {Math.round(scale * 100)}%
          </button>
          <button
            type="button"
            aria-label="Zoom in diagram"
            title="Zoom in"
            onClick={() => zoom(1)}
          >
            <ZoomIn size={13} />
          </button>
          <button
            type="button"
            ref={toggle}
            aria-label={expanded
              ? "Close full screen diagram"
              : "Open diagram full screen"}
            title={expanded ? "Close full screen" : "Full screen"}
            onClick={() => setExpanded((value) => !value)}
          >
            {expanded ? <X size={13} /> : <Maximize2 size={13} />}
          </button>
        </div>
      )}
      <div
        ref={scrollFrame}
        className="markdownDiagramScroll"
        tabIndex={0}
        onPointerDown={startPan}
        onPointerMove={movePan}
        onPointerUp={endPan}
        onPointerCancel={endPan}
      >
        {current ? (
          <div
            className="markdownDiagramStage"
            style={base ? {
              width: base.width * scale,
              height: base.height * scale
            } : undefined}
          >
            <div
              className="markdownDiagramZoom"
              style={base ? {
                width: base.width,
                height: base.height,
                transform: `scale(${scale})`
              } : undefined}
            >
              <div
                ref={container}
                className="markdownDiagramSvg"
                dangerouslySetInnerHTML={{__html: current.svg}}
              />
            </div>
          </div>
        ) : (
          <span
            className="markdownImageState"
            role="status"
          >
            <LoaderCircle
              className="spin"
              size={15}
            /> Rendering diagram
          </span>
        )}
      </div>
    </div>
  );
  return expanded ? createPortal(shell, document.body) : shell;
}

// The Web host serves a strict style-src 'self' policy, so mermaid's inline
// style attributes and <style> blocks are reported and dropped. Re-applying
// them through the CSSOM (element.style.cssText and a constructed stylesheet)
// restores the theme without relaxing the Content-Security-Policy.
function applyDiagramStyles(root: HTMLElement): CSSStyleSheet | null {
  try {
    for (const element of root.querySelectorAll<SVGElement>("[style]")) {
      const css = element.getAttribute("style");
      if (css) element.style.cssText = css;
    }
    const rules = [...root.querySelectorAll("style")]
      .map((node) => node.textContent ?? "")
      .filter(Boolean)
      .join("\n");
    if (!rules || typeof CSSStyleSheet === "undefined") return null;
    const sheet = new CSSStyleSheet();
    sheet.replaceSync(rules);
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
    return sheet;
  } catch {
    return null;
  }
}

function detachDiagramSheet(sheet: CSSStyleSheet): void {
  try {
    document.adoptedStyleSheets = document.adoptedStyleSheets.filter(
      (value) => value !== sheet
    );
  } catch {
    // Document teardown races unmount; a detached sheet is garbage-collected.
  }
}
