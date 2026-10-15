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
