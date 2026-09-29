/**
 * RequestScope is one epoch of client work: a connection, a Session
 * selection, a Session list query or a start attempt. Work started under a
 * scope may publish only while the scope is live; ending the scope aborts its
 * in-flight requests and turns their late results into no-ops. Every
 * asynchronous continuation checks `live` after each await instead of
 * comparing ad-hoc generation counters.
 */
export class RequestScope {
  private readonly controller = new AbortController();

  get signal(): AbortSignal {
    return this.controller.signal;
  }

  get live(): boolean {
    return !this.controller.signal.aborted;
  }

  end(): void {
    this.controller.abort();
  }
}

/** ScopeSlot owns the single live scope of one kind of client work. */
export class ScopeSlot {
  private scope = new RequestScope();

  get current(): RequestScope {
    return this.scope;
  }

  /** Ends the current scope and opens its successor. */
  renew(): RequestScope {
    this.scope.end();
    this.scope = new RequestScope();
    return this.scope;
  }
}
