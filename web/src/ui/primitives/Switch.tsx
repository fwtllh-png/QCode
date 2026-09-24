import type { ReactNode } from "react";

// 布尔偏好开关：真实按钮承载 role="switch"，避免原生 checkbox 的
// 平台外观与整体视觉割裂。键盘操作走按钮默认行为（Enter/Space 触发）。
export function Switch({
  label,
  checked,
  disabled,
  title,
  onChange,
  children
}: {
  label: string;
  checked: boolean;
  disabled?: boolean;
  title?: string;
  onChange: (next: boolean) => void;
  children?: ReactNode;
}) {
  return (
    <button
      type="button"
      role="switch"
      className="chSwitch"
      data-on={checked || undefined}
      aria-label={label}
      aria-checked={checked}
      disabled={disabled}
      title={title}
      onClick={() => onChange(!checked)}
    >
      <span className="chSwitchTrack">
        <span className="chSwitchThumb" />
      </span>
      {children}
    </button>
  );
}
