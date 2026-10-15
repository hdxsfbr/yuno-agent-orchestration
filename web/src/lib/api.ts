// Thin fetch wrapper matching the API's one error shape, plus an SSE client built on
// fetch so it can send headers (Last-Event-ID, Authorization) that EventSource cannot.

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details?: unknown,
  ) {
    super(message);
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let body: unknown = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = text;
  }
  if (!res.ok) {
    const err = (body as { error?: { code?: string; message?: string; details?: unknown } })?.error;
    throw new ApiError(res.status, err?.code ?? "http_error", err?.message ?? res.statusText, err?.details);
  }
  return body as T;
}

export type SSEEvent = { id?: string; event: string; data: unknown };

/** Parses the text/event-stream wire format incrementally. */
export function createSSEParser(onEvent: (e: SSEEvent) => void) {
  let buffer = "";
  let id: string | undefined;
  let event = "message";
  let data: string[] = [];

  const dispatch = () => {
    if (data.length > 0) {
      const raw = data.join("\n");
      let parsed: unknown = raw;
      try {
        parsed = JSON.parse(raw);
      } catch {
        /* keep raw */
      }
      onEvent({ id, event, data: parsed });
    }
    event = "message";
    data = [];
  };

  return {
    feed(chunk: string) {
      buffer += chunk;
      let idx: number;
      while ((idx = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, idx).replace(/\r$/, "");
        buffer = buffer.slice(idx + 1);
        if (line === "") {
          dispatch();
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let value = colon < 0 ? "" : line.slice(colon + 1);
        if (value.startsWith(" ")) value = value.slice(1);
        if (field === "event") event = value;
        else if (field === "data") data.push(value);
        else if (field === "id") id = value;
      }
    },
    lastId: () => id,
  };
}

/**
 * Subscribes to an SSE endpoint. Reconnects with Last-Event-ID after a 1 s pause.
 * Returns an unsubscribe function.
 */
export function subscribe(path: string, onEvent: (e: SSEEvent) => void, onStatus?: (s: "open" | "closed") => void) {
  const controller = new AbortController();
  let lastId: string | undefined;
  let stopped = false;

  const connect = async () => {
    while (!stopped) {
      try {
        const res = await fetch(path, {
          headers: { Accept: "text/event-stream", ...(lastId ? { "Last-Event-ID": lastId } : {}) },
          signal: controller.signal,
        });
        if (!res.ok || !res.body) throw new Error(`sse ${res.status}`);
        onStatus?.("open");
        const parser = createSSEParser((e) => {
          if (e.id) lastId = e.id;
          onEvent(e);
        });
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          parser.feed(decoder.decode(value, { stream: true }));
        }
      } catch {
        /* fall through to reconnect */
      }
      onStatus?.("closed");
      if (stopped) return;
      await new Promise((r) => setTimeout(r, 1000));
    }
  };
  void connect();
  return () => {
    stopped = true;
    controller.abort();
  };
}
