import { describe, expect, it } from "vitest";
import { createSSEParser, delegatedConversationId, isDelegatedRun, type SSEEvent } from "./api";

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

describe("delegatedConversationId", () => {
  it("reads the conversation from a channel run's reference", () => {
    expect(delegatedConversationId({ trigger: "channel", trigger_ref: "conversation:c1" })).toBe("c1");
    expect(delegatedConversationId({ trigger: "channel", trigger_ref: " conversation:0192-ab " })).toBe("0192-ab");
  });

  it("has none for a delegated run without a conversation, or a reference it does not know", () => {
    expect(delegatedConversationId({ trigger: "channel", trigger_ref: "" })).toBeUndefined();
    expect(delegatedConversationId({ trigger: "channel" })).toBeUndefined();
    expect(delegatedConversationId({ trigger: "channel", trigger_ref: "conversation:" })).toBeUndefined();
    expect(delegatedConversationId({ trigger: "channel", trigger_ref: "telegram:1001" })).toBeUndefined();
  });

  it("has none for runs that were not delegated", () => {
    expect(delegatedConversationId({ trigger: "manual", trigger_ref: "conversation:c1" })).toBeUndefined();
    expect(delegatedConversationId({ trigger: "schedule", trigger_ref: "s1@2026-10-16T09:00:00Z" })).toBeUndefined();
  });
});

describe("isDelegatedRun", () => {
  it("is true for a channel run from a conversation or with no reference", () => {
    expect(isDelegatedRun({ trigger: "channel", trigger_ref: "conversation:c1" })).toBe(true);
    expect(isDelegatedRun({ trigger: "channel", trigger_ref: "" })).toBe(true);
    expect(isDelegatedRun({ trigger: "channel" })).toBe(true);
  });

  it("is false for a channel run with a reference that names no conversation, and for other triggers", () => {
    expect(isDelegatedRun({ trigger: "channel", trigger_ref: "telegram:1001" })).toBe(false);
    expect(isDelegatedRun({ trigger: "channel", trigger_ref: "conversation:" })).toBe(false);
    expect(isDelegatedRun({ trigger: "manual" })).toBe(false);
  });
});
