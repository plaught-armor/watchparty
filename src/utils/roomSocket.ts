type Handler = (data?: any) => void;

export interface RoomSocketOptions {
  clientId: string;
  sessionId: string;
}

// Close code the server uses when it removes this client on purpose.
const CLOSE_KICKED = 4000;
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 5000;

/**
 * Connection to one room on the WatchParty server.
 *
 * Frames are JSON arrays of [event, data] in both directions. Besides the
 * server's own events, handlers receive "connect", "connect_error" (with
 * `{ message }`, final) and "disconnect" (with a reason string; the reason
 * "io server disconnect" means the server ended the session and there will
 * be no reconnect).
 */
export class RoomSocket {
  private ws: WebSocket | null = null;
  private handlers = new Map<string, Set<Handler>>();
  private retryDelay = RECONNECT_MIN_MS;
  private retryTimer: number | undefined;
  private closed = false;
  private url: string;
  private options: RoomSocketOptions;

  /** `url` is the full ws:// or wss:// address of the room. */
  constructor(url: string, options: RoomSocketOptions) {
    this.url = url;
    this.options = options;
    this.open();
  }

  on(event: string, handler: Handler): void {
    let set = this.handlers.get(event);
    if (!set) {
      set = new Set();
      this.handlers.set(event, set);
    }
    set.add(handler);
  }

  off(event: string, handler: Handler): void {
    this.handlers.get(event)?.delete(handler);
  }

  /** Sends an event. Dropped while disconnected: room state is resent on reconnect. */
  emit(event: string, data?: unknown): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(
        JSON.stringify(data === undefined ? [event] : [event, data]),
      );
    }
  }

  /** Closes the connection for good. */
  close(): void {
    this.closed = true;
    window.clearTimeout(this.retryTimer);
    this.ws?.close();
  }

  private dispatch(event: string, data?: unknown): void {
    // Copy so a handler may unsubscribe while we iterate.
    for (const handler of [...(this.handlers.get(event) ?? [])]) {
      handler(data);
    }
  }

  private open(): void {
    const ws = new WebSocket(this.url);
    this.ws = ws;
    ws.onopen = () => {
      ws.send(JSON.stringify(this.options));
    };
    ws.onmessage = (message) => {
      let frame: unknown;
      try {
        frame = JSON.parse(message.data);
      } catch {
        console.warn("[RoomSocket] ignoring malformed frame");
        return;
      }
      if (!Array.isArray(frame) || typeof frame[0] !== "string") {
        return;
      }
      if (frame[0] === "connect") {
        this.retryDelay = RECONNECT_MIN_MS;
      } else if (frame[0] === "connect_error") {
        // The server refused us; retrying would get the same answer.
        this.closed = true;
      }
      this.dispatch(frame[0], frame[1]);
    };
    ws.onclose = (event) => {
      if (this.closed) {
        return;
      }
      if (event.code === CLOSE_KICKED) {
        this.closed = true;
        this.dispatch("disconnect", "io server disconnect");
        return;
      }
      this.dispatch("disconnect", "transport close");
      this.retryTimer = window.setTimeout(() => this.open(), this.retryDelay);
      this.retryDelay = Math.min(this.retryDelay * 2, RECONNECT_MAX_MS);
    };
  }
}
