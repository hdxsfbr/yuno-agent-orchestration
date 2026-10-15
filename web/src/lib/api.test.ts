import { describe, expect, it } from "vitest";
import { createSSEParser, type SSEEvent } from "./api";

describe("createSSEParser", () => {
  it("parses events split across chunks and ignores comments", () => {
    const got: SSEEvent[] = [];
    const p = createSSEParser((e) => got.push(e));
    p.feed(": connected\n\nid: 7\nevent: run.status\nda");
    p.feed('ta: {"status":"running"}\n\n');
    p.feed("event: plain\ndata: hello\n\n");
    expect(got).toEqual([
      { id: "7", event: "run.status", data: { status: "running" } },
      { id: "7", event: "plain", data: "hello" },
    ]);
    expect(p.lastId()).toBe("7");
  });

  it("joins multi-line data and handles CRLF", () => {
    const got: SSEEvent[] = [];
    const p = createSSEParser((e) => got.push(e));
    p.feed("data: a\r\ndata: b\r\n\r\n");
    expect(got[0].data).toBe("a\nb");
  });
});
