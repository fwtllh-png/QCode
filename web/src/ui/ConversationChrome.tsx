import {
  Check,
  Copy,
  ThumbsDown,
  ThumbsUp
} from "lucide-react";
import {
  useEffect,
  useRef,
  useState,
  type CSSProperties
} from "react";

export type MessageFeedbackRating = "positive" | "negative";

export interface MessageChrome {
  completedAt?: string;
  totalMS?: number;
  firstTokenMS?: number;
  tokensPerSecond?: number;
}

const messageTimeFormat = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  hour12: false
});

export function MessageActions({
  text,
  chrome,
  feedback,
  onFeedback
}: {
  text: string;
  chrome?: MessageChrome;
  feedback?: MessageFeedbackRating;
  onFeedback: (rating: MessageFeedbackRating) => void;
}) {
  const [copied, setCopied] = useState(false);
  const copyTimer = useRef<number>();

  useEffect(() => () => {
    if (copyTimer.current !== undefined) {
      window.clearTimeout(copyTimer.current);
    }
  }, []);

  const copy = async () => {
    if (copied) return;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      return;
    }
    setCopied(true);
    copyTimer.current = window.setTimeout(() => setCopied(false), 1_000);
  };

  const facts = [
    chrome?.completedAt ? formatMessageTime(chrome.completedAt) : "",
    chrome?.totalMS !== undefined
      ? `Ran for ${formatRunDuration(chrome.totalMS)}`
      : "",
    chrome?.firstTokenMS !== undefined
      ? `${formatLatency(chrome.firstTokenMS)} TTFT`
      : "",
    chrome?.tokensPerSecond !== undefined
      ? `~${formatThroughput(chrome.tokensPerSecond)} tok/s`
      : ""
  ].filter(Boolean);

  return (
    <div className="messageActions" aria-label="Message actions">
      <button
        type="button"
        aria-label={copied ? "Copied" : "Copy response"}
        title={copied ? "Copied" : "Copy response"}
        onClick={() => void copy()}
      >
        {copied ? <Check size={16} /> : <Copy size={16} />}
      </button>
      <button
        type="button"
        aria-label={feedback === "positive" ? "Remove like" : "Like response"}
        aria-pressed={feedback === "positive"}
        title={feedback === "positive" ? "Remove like" : "Like response"}
        onClick={() => onFeedback("positive")}
      >
        <ThumbsUp size={16} />
      </button>
      <button
        type="button"
        aria-label={feedback === "negative" ? "Remove dislike" : "Dislike response"}
        aria-pressed={feedback === "negative"}
        title={feedback === "negative" ? "Remove dislike" : "Dislike response"}
        onClick={() => onFeedback("negative")}
      >
        <ThumbsDown size={16} />
      </button>
      {facts.length > 0 && (
        <span className="messageFacts">{facts.join("  ·  ")}</span>
      )}
    </div>
  );
}

export interface ContextAttribution {
  estimatedTokens: number;
  stableTokens: number;
  toolTokens: number;
  messageTokens: number;
  framingTokens: number;
}

function formatMessageTime(value: string): string {
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) return "";
  return messageTimeFormat.format(parsed);
}

function formatRunDuration(value: number): string {
  const seconds = Math.max(0, Math.round(value / 1_000));
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, "0")}s`;
}

function formatLatency(value: number): string {
  const seconds = Math.max(0, value) / 1_000;
  return `${seconds < 10 ? Math.round(seconds * 10) / 10 : Math.round(seconds)}s`;
}

function formatThroughput(value: number): string {
  return value >= 10 ? String(Math.round(value)) : String(Math.round(value * 10) / 10);
}

export function compactSelectWidth(value: string): CSSProperties {
  const characters = Math.max(3, Math.min(18, [...value].length));
  return {width: `calc(${characters}ch + 24px)`};
}
