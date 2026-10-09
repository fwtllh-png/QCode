import {
  Check,
  ChevronDown,
  ChevronUp
} from "lucide-react";
import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent
} from "react";
import {Presence} from "./primitives/Presence";

interface Props {
  value: string;
  defaultValue?: string;
  values: readonly string[];
  disabled?: boolean;
  onChange: (value: string) => void;
}

export function ReasoningMenu({
  value,
  defaultValue = "",
  values,
  disabled,
  onChange
}: Props) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLSpanElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const selected = values.includes(value)
    ? value
    : values.includes(defaultValue)
      ? defaultValue
      : values[0] ?? "";

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (event.target instanceof Node && rootRef.current?.contains(event.target)) {
        return;
      }
      setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const frame = requestAnimationFrame(() => {
      const current = menuRef.current?.querySelector<HTMLButtonElement>(
        '[role="menuitemradio"][aria-checked="true"]'
      );
      (current ?? menuRef.current?.querySelector<HTMLButtonElement>("button"))?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [open]);

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      setOpen(false);
      triggerRef.current?.focus();
      return;
    }
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const items = Array.from(
      menuRef.current?.querySelectorAll<HTMLButtonElement>("button") ?? []
    );
    if (items.length === 0) return;
    const current = items.indexOf(document.activeElement as HTMLButtonElement);
    let next = event.key === "Home" ? 0 : items.length - 1;
    if (event.key === "ArrowDown") next = (current + 1) % items.length;
    if (event.key === "ArrowUp") {
      next = (current - 1 + items.length) % items.length;
    }
    items[next]?.focus();
  };

  return (
    <span className="reasoningMenuRoot" ref={rootRef}>
      <button
        ref={triggerRef}
        type="button"
        className="reasoningMenuTrigger"
        aria-label="Reasoning"
        aria-haspopup="menu"
        aria-expanded={open}
        disabled={disabled}
        title={`Reasoning: ${reasoningLabel(selected)}`}
        onClick={() => setOpen((current) => !current)}
        onKeyDown={(event) => {
          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
          event.preventDefault();
          setOpen(true);
        }}
      >
        <span>{reasoningLabel(selected)}</span>
        {open
          ? <ChevronUp size={16} aria-hidden="true" />
          : <ChevronDown size={16} aria-hidden="true" />}
      </button>
      <Presence open={open}>
        <div
          ref={menuRef}
          className="reasoningMenu"
          data-motion-surface
          role="menu"
          aria-label="Reasoning modes"
          onKeyDown={onMenuKeyDown}
        >
          {values.map((effort) => (
            <button
              key={effort}
              type="button"
              role="menuitemradio"
              aria-checked={effort === selected}
              data-selected={effort === selected || undefined}
              onClick={() => {
                setOpen(false);
                if (effort !== value) onChange(effort);
                triggerRef.current?.focus();
              }}
            >
              <span>{reasoningLabel(effort)}</span>
              {effort === selected && <Check size={16} aria-hidden="true" />}
            </button>
          ))}
        </div>
      </Presence>
    </span>
  );
}

function reasoningLabel(value: string): string {
  if (!value) return "Default";
  if (value === "xhigh") return "XHigh";
  return value.charAt(0).toUpperCase() + value.slice(1);
}
