export const experience = Object.freeze({
  layout: {
    sidebarCollapsed: 56,
    sidebarDefault: 280,
    sidebarMinimum: 264,
    sidebarMaximum: 420,
    centerIdealMinimum: 640,
    chatContent: 748,
    composer: 920,
    disclosureRow: 24,
    trajectoryRow: 30,
    compactBreakpoint: 720
  },
  scrolling: {
    // 跟随底部带滞回：已跟随时要滚离 160px 才脱离，脱离后回到 48px 内重新跟随。
    // 单一窄阈值会让一格滚轮就永久断跟随，被感知为“内容卡住/跳变”。
    followEnterThreshold: 48,
    followLeaveThreshold: 160
  },
  trajectory: {
    overscanViewports: 2,
    minimumDOMBudget: 72,
    spanMinimumPixels: 2,
    initialViewportHeight: 600,
    dragThresholdFraction: 0.003,
    minimumZoomOperations: 4,
    zoomInFactor: 0.8,
    zoomOutFactor: 1.25
  }
});

export function trajectoryDOMBudget(viewportHeight: number): number {
  const visibleRows = Math.max(
    1,
    Math.ceil(viewportHeight / experience.layout.trajectoryRow)
  );
  return Math.max(
    experience.trajectory.minimumDOMBudget,
    visibleRows * (1 + experience.trajectory.overscanViewports * 2)
  );
}
