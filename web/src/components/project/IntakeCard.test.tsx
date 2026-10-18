// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Project } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { intake, project } from "@/test/fixtures";
import IntakeCard from "./IntakeCard";

let api: ReturnType<typeof installFakeApi>;

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

beforeEach(() => {
  api = installFakeApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderCard(p: Project) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <IntakeCard project={p} />
    </QueryClientProvider>,
  );
  return { card: () => within(screen.getByRole("region", { name: "Issue intake" })), client };
}

const pollButton = () => screen.getByRole("button", { name: /^(Poll now|Polling…)$/ }) as HTMLButtonElement;

describe("the settings line", () => {
  it("says Off when the intake is off, with Poll now disabled", () => {
    const { card } = renderCard(project({ intake: intake({ enabled: false }) }));
    expect(card().getByText("Off")).not.toBeNull();
    expect(pollButton().disabled).toBe(true);
    expect(pollButton().title).toBe("Intake is off");
  });

  it("says On with the label and the workflow's name", () => {
    const { card } = renderCard(project({ intake: intake({ label: "robot", workflow_name: "Dev pipeline (ours)" }) }));
    expect(card().getByText("On: label robot, workflow Dev pipeline (ours)")).not.toBeNull();
    expect(pollButton().disabled).toBe(false);
    expect(pollButton().title).toBe("");
  });

  it("says there is no workflow when the one it ran was deleted", () => {
    const { card } = renderCard(project({ intake: intake({ workflow_id: null, workflow_name: null, last_error: "intake: no workflow" }) }));
    expect(card().getByText("On: label yuno, no workflow")).not.toBeNull();
    expect(card().getByText("intake: no workflow")).not.toBeNull();
  });

  it("says Off for a project the API sent no settings for", () => {
    const { card } = renderCard(project());
    expect(card().getByText("Off")).not.toBeNull();
    expect(card().getByText("Not polled yet")).not.toBeNull();
    expect(pollButton().disabled).toBe(true);
    expect(pollButton().title).toBe("Intake is off");
  });
});

describe("the last poll", () => {
  it("says Not polled yet before the first poll", () => {
    const { card } = renderCard(project({ intake: intake() }));
    expect(card().getByText("Not polled yet")).not.toBeNull();
    expect(card().queryByText(/^Last poll/)).toBeNull();
  });

  it.each([
    [2_000, "Last poll just now"],
    [42_000, "Last poll 42s ago"],
    [5 * 60_000, "Last poll 5 min ago"],
    [3 * 3_600_000, "Last poll 3 h ago"],
  ])("tells a poll %i ms ago as %s", (ms, text) => {
    const { card } = renderCard(project({ intake: intake({ polled_at: ago(ms) }) }));
    expect(card().getByText(text)).not.toBeNull();
    expect(card().queryByText("Not polled yet")).toBeNull();
  });

  it("shows the last error as a muted line, and none when the last poll was clean", () => {
    const { card } = renderCard(project({ intake: intake({ polled_at: ago(60_000), last_error: "project yuno-playground has reached its daily budget ($5.00)" }) }));
    const line = card().getByText("project yuno-playground has reached its daily budget ($5.00)");
    expect(line.className).toContain("zinc-500");
    expect(line.getAttribute("role")).toBeNull();
    cleanup();
    const clean = renderCard(project({ intake: intake({ polled_at: ago(60_000), last_error: null }) }));
    expect(clean.card().queryByText(/budget/)).toBeNull();
  });
});

describe("Poll now", () => {
  it("has its status region in the page, empty, before any poll", () => {
    const { card } = renderCard(project({ intake: intake() }));
    expect(card().getByRole("status").textContent).toBe("");
  });

  it("polls the project with POST and no body, and says how many runs it started and how many finished ones it reported", async () => {
    api.on("POST", "/api/projects/p1/intake/poll", () => ({ taken: 3, closed: 0 }));
    const { card } = renderCard(project({ intake: intake() }));
    const region = card().getByRole("status");
    fireEvent.click(pollButton());
    await waitFor(() => expect(region.textContent).toBe("Started 3 runs, reported 0 finished"));
    // The region that was in the page before the answer is the one that holds it.
    expect(card().getByRole("status")).toBe(region);
    expect(api.made("POST", "/api/projects/p1/intake/poll")).toHaveLength(1);
    expect(api.made("POST", "/api/projects/p1/intake/poll")[0].body).toBeUndefined();
  });

  it.each([
    [1, 1, "Started 1 run, reported 1 finished"],
    [0, 1, "Started 0 runs, reported 1 finished"],
    [2, 0, "Started 2 runs, reported 0 finished"],
    [0, 0, "Started 0 runs, reported 0 finished"],
  ])("tells %i taken and %i closed as %s", async (taken, closed, text) => {
    api.on("POST", "/api/projects/p1/intake/poll", () => ({ taken, closed }));
    const { card } = renderCard(project({ intake: intake() }));
    fireEvent.click(pollButton());
    await waitFor(() => expect(card().getByRole("status").textContent).toBe(text));
  });

  it("is busy while the poll runs", async () => {
    let release: (r: { taken: number; closed: number }) => void = () => undefined;
    api.on("POST", "/api/projects/p1/intake/poll", () => new Promise((r) => (release = r)));
    renderCard(project({ intake: intake() }));
    fireEvent.click(pollButton());
    await waitFor(() => expect(pollButton().textContent).toBe("Polling…"));
    expect(pollButton().disabled).toBe(true);
    expect(pollButton().title).toBe("");
    release({ taken: 0, closed: 0 });
    await waitFor(() => expect(pollButton().textContent).toBe("Poll now"));
  });

  it("invalidates the project and its runs once the pass is done", async () => {
    api.on("POST", "/api/projects/p1/intake/poll", () => ({ taken: 1, closed: 1 }));
    const { client } = renderCard(project({ intake: intake() }));
    const invalidated = vi.spyOn(client, "invalidateQueries");
    fireEvent.click(pollButton());
    await waitFor(() => expect(invalidated).toHaveBeenCalledTimes(2));
    expect(invalidated.mock.calls.map((c) => c[0]?.queryKey)).toEqual([["projects"], ["runs"]]);
  });

  it("shows the server's own message for a 409 and no counts", async () => {
    api.on("POST", "/api/projects/p1/intake/poll", () => apiError(409, "intake_disabled", "issue intake is off for this project"));
    const { card } = renderCard(project({ intake: intake() }));
    fireEvent.click(pollButton());
    expect((await card().findByRole("alert")).textContent).toBe("issue intake is off for this project");
    expect(card().getByRole("status").textContent).toBe("");
    expect(pollButton().disabled).toBe(false);
    expect(pollButton().title).toBe("");
  });

  it("says what failed for any other error", async () => {
    api.on("POST", "/api/projects/p1/intake/poll", () => apiError(500, "internal", "boom"));
    const { card } = renderCard(project({ intake: intake() }));
    fireEvent.click(pollButton());
    expect((await card().findByRole("alert")).textContent).toBe("Could not poll the project: boom");
  });

  it("replaces the counts of an earlier poll and drops an earlier error when polled again", async () => {
    let answers = [() => apiError(500, "internal", "boom"), () => ({ taken: 1, closed: 2 })];
    api.on("POST", "/api/projects/p1/intake/poll", () => answers.shift()!());
    const { card } = renderCard(project({ intake: intake() }));
    fireEvent.click(pollButton());
    await card().findByRole("alert");
    fireEvent.click(pollButton());
    await waitFor(() => expect(card().getByRole("status").textContent).toBe("Started 1 run, reported 2 finished"));
    expect(card().queryByRole("alert")).toBeNull();
    answers = [() => ({ taken: 0, closed: 0 })];
    fireEvent.click(pollButton());
    await waitFor(() => expect(card().getByRole("status").textContent).toBe("Started 0 runs, reported 0 finished"));
  });
});
