// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunStatusBadge, StatusBadge } from "./Badge";

afterEach(cleanup);

describe("RunStatusBadge", () => {
  it("shows the status as words", () => {
    render(<RunStatusBadge status="waiting_approval" />);
    expect(screen.getByText("waiting approval")).not.toBeNull();
  });

  it.each([
    ["queued", "bg-zinc-100"],
    ["running", "bg-blue-100"],
    ["waiting_approval", "bg-amber-100"],
    ["paused", "bg-amber-100"],
    ["succeeded", "bg-emerald-100"],
    ["failed", "bg-red-100"],
    ["cancelled", "bg-zinc-100"],
    ["timed_out", "bg-orange-100"],
  ])("colours %s with %s", (status, cls) => {
    render(<RunStatusBadge status={status} />);
    expect(screen.getByText(status.replace("_", " ")).className).toContain(cls);
  });

  it("marks only a running run as live", () => {
    const { container, rerender } = render(<RunStatusBadge status="running" />);
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
    rerender(<RunStatusBadge status="succeeded" />);
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });

  it("falls back to neutral for a status it does not know", () => {
    render(<StatusBadge kind="run" status="mystery" />);
    expect(screen.getByText("mystery").className).toContain("bg-zinc-100");
  });
});

describe("StatusBadge for a branch", () => {
  it.each([
    ["running", "running", "bg-blue-100"],
    ["succeeded", "succeeded", "bg-emerald-100"],
    ["failed", "failed", "bg-red-100"],
    ["cancelled", "cancelled", "bg-zinc-100"],
    ["waiting_approval", "Waiting for an answer", "bg-amber-100"],
  ])("shows %s as %s in the colour %s", (status, words, cls) => {
    render(<StatusBadge kind="branch" status={status} />);
    expect(screen.getByText(words).className).toContain(cls);
  });

  it("keeps the plain words for a node that waits", () => {
    render(<StatusBadge kind="node" status="waiting_approval" />);
    expect(screen.getByText("waiting approval")).not.toBeNull();
    expect(screen.queryByText("Waiting for an answer")).toBeNull();
  });

  it("marks only a running branch as live", () => {
    const { container, rerender } = render(<StatusBadge kind="branch" status="running" />);
    expect(container.querySelector(".animate-pulse")).not.toBeNull();
    rerender(<StatusBadge kind="branch" status="waiting_approval" />);
    expect(container.querySelector(".animate-pulse")).toBeNull();
  });
});
