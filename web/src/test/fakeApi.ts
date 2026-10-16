import { vi } from "vitest";

// A fake API for page tests: handlers answer JSON requests by method and path, and every
// path that ends in /events is an event stream the test writes to.

export function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

export const apiError = (status: number, code: string, message: string, details?: unknown) => json({ error: { code, message, details } }, status);

/** Header names are lower-cased so a test does not depend on how the code spelled them. */
export type HeaderMap = Record<string, string>;
export type Call = { method: string; url: string; body: unknown; headers: HeaderMap };
type Request = Call & { match: RegExpMatchArray };
type Handler = (req: Request) => unknown | Response | Promise<unknown | Response>;

const encoder = new TextEncoder();

const lowerCased = (h?: HeadersInit): HeaderMap => Object.fromEntries(new Headers(h).entries());

export function installFakeApi() {
  const calls: Call[] = [];
  const streamCalls: Call[] = [];
  const handlers: { method: string; pattern: RegExp; fn: Handler }[] = [];
  const streams = new Map<string, ReadableStreamDefaultController<Uint8Array>>();
  let eventId = 0;
  let requiredToken: string | undefined;

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      const url = String(input);
      const headers = lowerCased(init?.headers);
      const isStream = url.split("?")[0].endsWith("/events");
      // Like the real API with API_TOKEN set: every call without the bearer is refused.
      if (requiredToken !== undefined && headers.authorization !== `Bearer ${requiredToken}`) {
        (isStream ? streamCalls : calls).push({ method, url, body: undefined, headers });
        return apiError(401, "unauthorized", "missing or invalid bearer token");
      }
      if (isStream) {
        const path = url.split("?")[0];
        streamCalls.push({ method, url, body: undefined, headers });
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            streams.set(path, c);
            c.enqueue(encoder.encode(": connected\n\n"));
            init?.signal?.addEventListener("abort", () => {
              if (streams.get(path) === c) streams.delete(path);
              try {
                c.close();
              } catch {
                /* already closed */
              }
            });
          },
        });
        return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
      }
      let body: unknown;
      if (typeof init?.body === "string") body = JSON.parse(init.body);
      calls.push({ method, url, body, headers });
      for (let i = handlers.length - 1; i >= 0; i--) {
        const h = handlers[i];
        const match = h.pattern.exec(url);
        if (h.method === method && match) {
          const out = await h.fn({ method, url, body, headers, match });
          return out instanceof Response ? out : json(out);
        }
      }
      return apiError(404, "not_found", `no fake for ${method} ${url}`);
    }),
  );

  return {
    calls,
    /** Every attempt to open an event stream, refused ones included. */
    streamCalls,
    /** Later registrations win, so a test can override a default. */
    on(method: string, pattern: RegExp | string, fn: Handler) {
      handlers.push({ method: method.toUpperCase(), pattern: typeof pattern === "string" ? new RegExp(`^${pattern.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`) : pattern, fn });
    },
    /** From now on, requests and streams must carry `Authorization: Bearer <token>`. */
    requireToken(token: string) {
      requiredToken = token;
    },
    /** The calls made with this method whose url matches. */
    made(method: string, pattern: RegExp | string): Call[] {
      return calls.filter((c) => c.method === method.toUpperCase() && (typeof pattern === "string" ? c.url === pattern : pattern.test(c.url)));
    },
    /** Writes one event to the stream at `path`. */
    send(path: string, event: string, data: unknown) {
      const c = streams.get(path);
      if (!c) throw new Error(`no open stream at ${path}`);
      c.enqueue(encoder.encode(`id: ${++eventId}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
    },
    hasStream: (path: string) => streams.has(path),
  };
}

/** What React Flow needs from the browser that jsdom lacks. */
export function mockReactFlowEnvironment() {
  class ResizeObserverStub {
    constructor(private cb: ResizeObserverCallback) {}
    observe(target: Element) {
      this.cb([{ target, contentRect: { width: 800, height: 600 } } as ResizeObserverEntry], this as unknown as ResizeObserver);
    }
    unobserve() {}
    disconnect() {}
  }
  class DOMMatrixReadOnlyStub {
    m22 = 1;
    constructor(transform?: string) {
      const scale = transform?.match(/scale\(([1-9.])\)/)?.[1];
      if (scale !== undefined) this.m22 = +scale;
    }
  }
  vi.stubGlobal("ResizeObserver", ResizeObserverStub);
  vi.stubGlobal("DOMMatrixReadOnly", DOMMatrixReadOnlyStub);
  Object.defineProperties(HTMLElement.prototype, {
    offsetHeight: { configurable: true, get: () => 40 },
    offsetWidth: { configurable: true, get: () => 200 },
  });
  (SVGElement.prototype as unknown as { getBBox: () => unknown }).getBBox = () => ({ x: 0, y: 0, width: 0, height: 0 });
}
