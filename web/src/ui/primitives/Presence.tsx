import {createContext, useContext, useLayoutEffect, useRef, type ReactNode} from "react";
import {usePresenceState} from "./motion";

const PresenceContext = createContext(true);

export function usePresenceActive() {
  return useContext(PresenceContext);
}

export function Presence({open, kind = "menu", backdrop, instantEnter, children}: {
  open: boolean;
  kind?: "dialog" | "menu" | "drawer" | "fade";
  backdrop?: boolean;
  // instantEnter：进场不做透明度/变换过渡（用于携带嵌套弹窗直接打开的
  // 宿主弹窗——父子分层动画在 WKWebView 中会合成竞争产生跳变）。
  instantEnter?: boolean;
  children: ReactNode;
}) {
  const parentActive = usePresenceActive();
  const ref = useRef<HTMLDivElement>(null);
  const previous = useRef<ReactNode>(null);
  const state = usePresenceState(open, ref);
  useLayoutEffect(() => {
    if (open) previous.current = children;
    else if (!state.present) previous.current = null;
  }, [open, children, state.present]);
  if (!state.present) return null;
  return (
    <PresenceContext.Provider value={parentActive && open}>
      <div
        ref={ref}
        className="uiPresence"
        data-modal-backdrop={backdrop || undefined}
        data-motion-kind={kind}
        data-presence={state.expanded ? "open" : "closed"}
        data-instant-enter={instantEnter || undefined}
        data-exiting={!open || undefined}
        aria-hidden={!open || undefined}
        {...(!open ? {inert: ""} : {})}
      >
        {open ? children : previous.current}
      </div>
    </PresenceContext.Provider>
  );
}
