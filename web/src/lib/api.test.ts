import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { projectSettings } from "@/test/fixtures";
import {
  ApiError,
  answerApproval,
  createSSEParser,
  delegatedConversationId,
  downloadArtifact,
  isDelegatedRun,
  issueRef,
  listRunArtifacts,
  type Approval,
  type Artifact,
  type SSEEvent,
} from "./api";
import { clearToken, setToken, signInRequired } from "./auth";

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

describe("issueRef", () => {
  const issue = (ref: string | undefined, project = projectSettings()) => ({ trigger: "issue" as const, trigger_ref: ref, project });

  it("reads the issue number from the reference and builds the GitLab address from the project's snapshot", () => {
    expect(issueRef(issue("p1#7"))).toEqual({ number: 7, url: "https://gitlab.com/andrebatista/yuno-playground/-/issues/7" });
  });

  it("builds the GitHub address without the dash", () => {
    const project = projectSettings({ provider: "github", repo_url: "https://github.com/acme/docs.git" });
    expect(issueRef(issue("0192-ab#31", project))).toEqual({ number: 31, url: "https://github.com/acme/docs/issues/31" });
  });

  it("takes the repository address with or without .git and with a trailing slash", () => {
    expect(issueRef(issue("p1#7", projectSettings({ repo_url: "https://gitlab.com/g/p" })))?.url).toBe("https://gitlab.com/g/p/-/issues/7");
    expect(issueRef(issue("p1#7", projectSettings({ repo_url: "https://gitlab.com/g/p.git/" })))?.url).toBe("https://gitlab.com/g/p/-/issues/7");
    expect(issueRef(issue("p1#7", projectSettings({ repo_url: "https://gitlab.example.com/a/b/c.git" })))?.url).toBe("https://gitlab.example.com/a/b/c/-/issues/7");
  });

  it("has the number but no address for a run that kept no snapshot of its project", () => {
    expect(issueRef({ trigger: "issue", trigger_ref: "p1#7" })).toEqual({ number: 7 });
  });

  it("is undefined for a run that did not come from an issue", () => {
    expect(issueRef({ trigger: "manual", trigger_ref: "p1#7", project: projectSettings() })).toBeUndefined();
    expect(issueRef({ trigger: "channel", trigger_ref: "conversation:c1" })).toBeUndefined();
  });

  it("is undefined for a reference that carries no issue number", () => {
    for (const ref of [undefined, "", "p1", "p1#", "#7", "p1#seven", "p1#0", "p1#7x", "p1#99999999999999999999"]) {
      expect(issueRef(issue(ref))).toBeUndefined();
    }
  });
});

describe("wave 10 calls", () => {
  let fake: ReturnType<typeof installFakeApi>;

  beforeEach(() => {
    fake = installFakeApi();
  });

  afterEach(() => {
    clearToken();
    vi.unstubAllGlobals();
  });

  const question = (over: Partial<Approval> = {}): Approval => ({
    id: "q/1",
    run_id: "r1",
    run_node_id: "rn1",
    kind: "question",
    node_id: "coder",
    message: "Which greeting language should I use?",
    options: ["English", "Spanish"],
    status: "approved",
    answer: "Spanish",
    requested_at: "2026-10-18T10:00:00Z",
    expires_at: "2026-10-19T10:00:00Z",
    ...over,
  });

  const artifact = (over: Partial<Artifact> = {}): Artifact => ({
    id: "a/1",
    run_id: "r/1",
    node_id: "writer",
    visit: 1,
    path: "report.md",
    size: 2048,
    sha256: "0".repeat(64),
    content_type: "text/markdown",
    generated: false,
    created_at: "2026-10-18T10:00:00Z",
    updated_at: "2026-10-18T10:01:00Z",
    ...over,
  });

  describe("answerApproval", () => {
    it("posts the answer to the question's answer address, encoding the id, and returns the decided record", async () => {
      fake.on("POST", "/api/approvals/q%2F1/answer", () => question());
      const decided = await answerApproval("q/1", { answer: "Spanish" });
      expect(decided).toMatchObject({ status: "approved", answer: "Spanish" });
      expect(fake.calls.map((c) => [c.method, c.url, c.body])).toEqual([["POST", "/api/approvals/q%2F1/answer", { answer: "Spanish" }]]);
    });

    it("raises a 422 with its details, and a 409 for a question decided already", async () => {
      fake.on("POST", "/api/approvals/q1/answer", () => apiError(422, "validation_failed", "invalid request", [{ path: "answer", message: "1 to 4096 characters" }]));
      const refused = await answerApproval("q1", { answer: "" }).catch((e: unknown) => e);
      expect(refused).toBeInstanceOf(ApiError);
      expect(refused).toMatchObject({ status: 422, code: "validation_failed", details: [{ path: "answer", message: "1 to 4096 characters" }] });

      fake.on("POST", "/api/approvals/q1/answer", () => apiError(409, "not_pending", "approval is already approved"));
      await expect(answerApproval("q1", { answer: "x" })).rejects.toMatchObject({ status: 409, code: "not_pending" });
    });
  });

  describe("listRunArtifacts", () => {
    it("reads the run's files, encoding the run id, and treats null as none", async () => {
      fake.on("GET", "/api/runs/r%2F1/artifacts", () => [artifact(), artifact({ id: "a2", path: "data/rows.csv" })]);
      expect((await listRunArtifacts("r/1")).map((a) => a.path)).toEqual(["report.md", "data/rows.csv"]);
      fake.on("GET", "/api/runs/r%2F1/artifacts", () => null);
      expect(await listRunArtifacts("r/1")).toEqual([]);
      fake.on("GET", "/api/runs/r%2F1/artifacts", () => []);
      expect(await listRunArtifacts("r/1")).toEqual([]);
    });

    it("raises a 404 for a run that does not exist", async () => {
      fake.on("GET", "/api/runs/nope/artifacts", () => apiError(404, "not_found", "run not found"));
      await expect(listRunArtifacts("nope")).rejects.toMatchObject({ status: 404, code: "not_found", message: "run not found" });
    });
  });

  describe("downloadArtifact", () => {
    const file = (body: BodyInit, type = "text/plain; charset=utf-8") => new Response(body, { status: 200, headers: { "Content-Type": type, "Content-Disposition": 'attachment; filename="report.md"' } });

    it("fetches the file with the token in a header and hands back its bytes as a blob", async () => {
      setToken("tok-1234");
      fake.on("GET", "/api/runs/r%2F1/artifacts/a%2F1", () => file("# Findings\n"));
      const blob = await downloadArtifact("r/1", "a/1");
      expect(await blob.text()).toBe("# Findings\n");
      expect(fake.calls).toHaveLength(1);
      expect(fake.calls[0].method).toBe("GET");
      // the token travels as a header, never in the address
      expect(fake.calls[0].headers.authorization).toBe("Bearer tok-1234");
      expect(fake.calls[0].url).toBe("/api/runs/r%2F1/artifacts/a%2F1");
    });

    it("keeps binary bytes as they are", async () => {
      const bytes = new Uint8Array([0, 255, 128, 10, 13, 0]);
      fake.on("GET", "/api/runs/r1/artifacts/a1", () => file(bytes, "application/octet-stream"));
      const blob = await downloadArtifact("r1", "a1");
      expect(blob.size).toBe(6);
      expect(new Uint8Array(await blob.arrayBuffer())).toEqual(bytes);
    });

    it("raises the API's error for a missing file with its code and message", async () => {
      fake.on("GET", "/api/runs/r1/artifacts/a1", () => apiError(404, "not_found", "artifact file is missing"));
      const err = await downloadArtifact("r1", "a1").catch((e: unknown) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect(err).toMatchObject({ status: 404, code: "not_found", message: "artifact file is missing" });
    });

    it("raises an ApiError for any answer that is not a 200, even when the body is not the API's error shape", async () => {
      fake.on("GET", "/api/runs/r1/artifacts/a1", () => new Response("<html>bad gateway</html>", { status: 502 }));
      const err = await downloadArtifact("r1", "a1").catch((e: unknown) => e);
      expect(err).toBeInstanceOf(ApiError);
      expect(err).toMatchObject({ status: 502, code: "http_error" });
      fake.on("GET", "/api/runs/r1/artifacts/a1", () => new Response(null, { status: 204 }));
      await expect(downloadArtifact("r1", "a1")).rejects.toMatchObject({ status: 204 });
    });

    it("asks for sign in when the token is refused", async () => {
      fake.requireToken("right");
      setToken("wrong");
      await expect(downloadArtifact("r1", "a1")).rejects.toMatchObject({ status: 401 });
      expect(signInRequired()).toBe(true);
    });
  });
});
