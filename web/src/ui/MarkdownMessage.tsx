import {
  Check,
  Copy,
  Download,
  ExternalLink,
  FileCode2,
  Image as ImageIcon,
  LoaderCircle,
  RotateCcw
} from "lucide-react";
import {
  isValidElement,
  lazy,
  memo,
  Suspense,
  useDeferredValue,
  useMemo,
  useState,
  type ReactNode
} from "react";
import ReactMarkdown, {type Components} from "react-markdown";
import remarkCjkFriendly from "remark-cjk-friendly/parseOnly";
import remarkGfm from "remark-gfm";
import {
  imageOrigin,
  isCrossOrigin,
  isExternalURL,
  safeImageSource,
  safeMarkdownURL,
  workspacePathFromHref
} from "./markdownURL";
import "./MarkdownMessage.css";

const MathMarkdownMessage = lazy(async () => ({
  default: (await import("./MathMarkdownMessage")).MathMarkdownMessage
}));

const MermaidDiagram = lazy(async () => ({
  default: (await import("./MermaidDiagram")).MermaidDiagram
}));

export const MarkdownMessage = memo(function MarkdownMessage({
  text,
  settled
}: {
  text: string;
  settled: boolean;
}) {
  const deferredText = useDeferredValue(text);
  const components = useMemo<Components>(() => ({
    a: ({href, children, ...properties}) => {
      const filePath = workspacePathFromHref(href);
      if (filePath) {
        return (
          <span
            className="markdownFileReference"
            title={filePath}
          >
            <FileCode2 size={14} aria-hidden="true" />
            <span>{children}</span>
          </span>
        );
      }
      if (!href) {
        return <span className="markdownUnsafeLink">{children}</span>;
      }
      const external = isExternalURL(href);
      return (
        <a
          {...properties}
          href={href}
          rel={external ? "noopener noreferrer" : undefined}
          target={external ? "_blank" : undefined}
        >
          {children}
        </a>
      );
    },
    img: ({src, alt}) => <MarkdownImage source={src} alt={alt ?? ""} />,
    pre: ({children}) => (
      <MarkdownCodeBlock settled={settled}>{children}</MarkdownCodeBlock>
    ),
    table: ({children}) => (
      <div
        className="markdownTable"
        role="region"
        aria-label="Response table"
        tabIndex={0}
      >
        <table>{children}</table>
      </div>
    )
  }), [settled]);
  if (settled && containsMath(text)) {
    return (
      <Suspense fallback={
        <BaseMarkdownMessage text={text} components={components} />
      }>
        <MathMarkdownMessage text={text} components={components} />
      </Suspense>
    );
  }
  return <BaseMarkdownMessage text={settled ? text : deferredText} components={components} />;
});

const BaseMarkdownMessage = memo(function BaseMarkdownMessage({
  text,
  components
}: {
  text: string;
  components: Components;
}) {
  return (
    <div className="assistantMarkdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm, remarkCjkFriendly]}
        components={components}
        urlTransform={safeMarkdownURL}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
});

function MarkdownCodeBlock({
  children,
  settled
}: {
  children?: ReactNode;
  settled: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const value = reactText(children).replace(/\n$/, "");
  const language = codeLanguage(children);
  const diagram = settled && language === "mermaid" && value.trim() !== "";
  const code = <pre tabIndex={0}>{children}</pre>;
  const copy = async () => {
    if (copied || !value) return;
    await navigator.clipboard.writeText(value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1_000);
  };
  return (
    <div
      className={diagram ? "markdownCodeBlock markdownDiagramBlock" : "markdownCodeBlock"}
      role="region"
      aria-label={diagram
        ? "Mermaid diagram"
        : language
          ? `${language} code`
          : "Code block"}
    >
      <div className="markdownCodeHeader">
        <span>{language || "Code"}</span>
        <button
          type="button"
          aria-label={copied ? "Copied code" : "Copy code"}
          title={copied ? "Copied" : "Copy code"}
          onClick={() => void copy()}
        >
          {copied ? <Check size={13} /> : <Copy size={13} />}
        </button>
      </div>
      {diagram ? (
        <Suspense fallback={code}>
          <MermaidDiagram
            source={value}
            fallback={code}
          />
        </Suspense>
      ) : code}
    </div>
  );
}

function MarkdownImage({
  source,
  alt
}: {
  source?: string;
  alt: string;
}) {
  const safeSource = safeImageSource(source);
  const remote = safeSource ? isCrossOrigin(safeSource) : false;
  const [requested, setRequested] = useState(!remote);
  const [state, setState] = useState<"loading" | "ready" | "error">(
    safeSource && !remote ? "loading" : "ready"
  );
  const [attempt, setAttempt] = useState(0);
  const label = alt.trim() || "Image";
  if (!safeSource) {
    return (
      <span className="markdownImageFallback" role="note">
        <ImageIcon size={14} aria-hidden="true" />
        {label}
      </span>
    );
  }
  if (!requested) {
    return (
      <button
        type="button"
        className="markdownImageConsent"
        aria-label={`Load image ${label}`}
        onClick={() => {
          setState("loading");
          setRequested(true);
        }}
      >
        <ImageIcon size={16} aria-hidden="true" />
        <span>{label}</span>
        <small>{imageOrigin(safeSource)} · Load image</small>
      </button>
    );
  }
  return (
    <span
      className="markdownImage"
      data-state={state}
      role="group"
      aria-label={`Image ${label}`}
    >
      <span className="markdownImageFrame">
        {state === "loading" && (
          <span className="markdownImageState" role="status">
            <LoaderCircle className="spin" size={15} /> Loading image
          </span>
        )}
        {state === "error" && (
          <span className="markdownImageState" role="alert">
            Image unavailable
          </span>
        )}
        <img
          key={attempt}
          src={safeSource}
          alt={label}
          loading="lazy"
          decoding="async"
          referrerPolicy="no-referrer"
          hidden={state === "error"}
          onLoad={() => setState("ready")}
          onError={() => setState("error")}
        />
      </span>
      <span className="markdownImageCaption">
        <span>{label}</span>
        <span className="markdownImageActions">
          {state === "error" && (
            <button
              type="button"
              aria-label={`Retry image ${label}`}
              title="Retry image"
              onClick={() => {
                setState("loading");
                setAttempt((value) => value + 1);
              }}
            >
              <RotateCcw size={13} />
            </button>
          )}
          <a
            href={safeSource}
            download
            target="_blank"
            rel="noopener noreferrer"
            aria-label={`Download image ${label}`}
            title="Download image"
          >
            <Download size={13} />
          </a>
          {remote && (
            <a
              href={safeSource}
              target="_blank"
              rel="noopener noreferrer"
              aria-label={`Open image ${label}`}
              title="Open image"
            >
              <ExternalLink size={13} />
            </a>
          )}
        </span>
      </span>
    </span>
  );
}

function codeLanguage(children: ReactNode): string {
  const child = Array.isArray(children) ? children[0] : children;
  if (!isValidElement<{className?: string}>(child)) return "";
  return /(?:^|\s)language-([^\s]+)/.exec(child.props.className ?? "")?.[1] ?? "";
}

function reactText(value: ReactNode): string {
  if (typeof value === "string" || typeof value === "number") {
    return String(value);
  }
  if (Array.isArray(value)) return value.map(reactText).join("");
  if (isValidElement<{children?: ReactNode}>(value)) {
    return reactText(value.props.children);
  }
  return "";
}

function containsMath(text: string): boolean {
  return /(^|[^\\])\$/.test(text);
}
