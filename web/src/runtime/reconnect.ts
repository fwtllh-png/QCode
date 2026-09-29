/**
 * Event-stream reconnect backoff. While the client runs it retries after an
 * unexpected close or a failed restart without a retry budget: the first
 * retry after a live connection drops waits the base delay, each consecutive
 * failure doubles it, and the cap bounds the wait so a restarted Supervisor
 * is picked up within one cap interval. Documented in docs/zh-CN/usage.md.
 */
export const reconnectBaseDelayMs = 700;
export const reconnectMaxDelayMs = 30_000;

/** Delay before the next start attempt after `failures` consecutive failures. */
export function reconnectDelay(failures: number): number {
  const exponent = failures > 0 ? Math.floor(failures) : 0;
  return Math.min(reconnectMaxDelayMs, reconnectBaseDelayMs * 2 ** exponent);
}
