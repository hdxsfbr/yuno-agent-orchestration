// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { CIStatus, MergeRequest, MergeRequestState, Run } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { mergeRequest } from "@/test/fixtures";
import MergeRequestCard from "./MergeRequestCard";

let api: ReturnType<typeof installFakeApi>;
let onChange: ReturnType<typeof vi.fn<(mr: MergeRequest) => void>>;

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

beforeEach(() => {
  api = installFakeApi();
  onChange = vi.fn<(mr: MergeRequest) => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderCard(mr: MergeRequest = mergeRequest(), run?: Pick<Run, "id" | "merge_request_id">) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <MergeRequestCard mr={mr} run={run} onChange={onChange} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...view, card: () => within(screen.getByRole("region", { name: /request/ })) };
}

const merge = () => screen.getByRole("button", { name: "Merge" }) as HTMLButtonElement;
const dialog = async () => within(await screen.findByRole("dialog"));

describe("the title line", () => {
  it("names a GitLab request a merge request and links its number and its title to the provider", () => {
    const { card } = renderCard();
    const number = card().getByRole("link", { name: "Merge request !12" });
    expect(number.getAttribute("href")).toBe("https://gitlab.com/andrebatista/yuno-playground/-/merge_requests/12");
    expect(number.getAttribute("target")).toBe("_blank");
    expect(number.getAttribute("rel")).toContain("noreferrer");
    const title = card().getByRole("link", { name: "Add the health endpoint" });
    expect(title.getAttribute("href")).toBe(number.getAttribute("href"));
    expect(card().getByText("yuno/0123456789ab → main")).not.toBeNull();
  });

  it("names a GitHub request a pull request", () => {
    const { card } = renderCard(mergeRequest({ provider: "github", number: 3, url: "https://github.com/o/r/pull/3", target_branch: "develop" }));
    expect(card().getByRole("link", { name: "Pull request #3" }).getAttribute("href")).toBe("https://github.com/o/r/pull/3");
    expect(screen.getByRole("region", { name: "Pull request" })).not.toBeNull();
    expect(card().getByText(/→ develop$/)).not.toBeNull();
  });
});

describe("the line for a run that updates the request", () => {
  const LINE = /This run updates it; opened by run/;

  it("says so, and links to the run that opened the request, when the run names it and did not open it", () => {
    const { card } = renderCard(mergeRequest({ id: "mr1", run_id: "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2" }), { id: "r2", merge_request_id: "mr1" });
    const line = card().getByText(LINE);
    expect(line.textContent).toBe("This run updates it; opened by run 5d4bb0f2");
    const link = within(line).getByRole("link");
    expect(link.getAttribute("href")).toBe("/runs/01a14161-d5ef-7d96-ba96-a45d5d4bb0f2");
    expect(link.getAttribute("title")).toBe("01a14161-d5ef-7d96-ba96-a45d5d4bb0f2");
  });

  it("is not there for the run that opened the request", () => {
    const { card } = renderCard(mergeRequest({ id: "mr1", run_id: "r1" }), { id: "r1", merge_request_id: undefined });
    expect(card().queryByText(LINE)).toBeNull();
  });

  it("is not there when the run names another request, none, or when the card is not told the run", () => {
    const { card } = renderCard(mergeRequest({ id: "mr1", run_id: "r0" }), { id: "r2", merge_request_id: "mr9" });
    expect(card().queryByText(LINE)).toBeNull();
    cleanup();
    expect(renderCard(mergeRequest({ id: "mr1", run_id: "r0" }), { id: "r2", merge_request_id: undefined }).card().queryByText(LINE)).toBeNull();
    cleanup();
    expect(renderCard(mergeRequest({ id: "mr1", run_id: "r0" })).card().queryByText(LINE)).toBeNull();
  });

  it("stays with a request that is final, since the run still updated it", () => {
    const { card } = renderCard(mergeRequest({ id: "mr1", run_id: "r0", state: "merged", closed_at: "2026-10-15T11:00:00Z" }), { id: "r2", merge_request_id: "mr1" });
    expect(card().getByText(LINE)).not.toBeNull();
  });
});

describe("badges", () => {
  it.each<[MergeRequestState, string]>([
    ["open", "Open"],
    ["merged", "Merged"],
    ["closed", "Closed"],
  ])("shows a %s request as %s", (state, label) => {
    const { card } = renderCard(mergeRequest({ state, closed_at: state === "open" ? undefined : "2026-10-15T11:00:00Z" }));
    expect(card().getByText(label)).not.toBeNull();
  });

  it.each<[CIStatus, string, string]>([
    ["none", "CI: None", "zinc"],
    ["pending", "CI: Pending", "amber"],
    ["passed", "CI: Passed", "emerald"],
    ["failed", "CI: Failed", "red"],
  ])("shows a pipeline that is %s as %s", (ci_status, label, tone) => {
    const { card } = renderCard(mergeRequest({ ci_status }));
    expect(card().getByText(label).className).toContain(tone);
  });

  it("tones the state: open blue, merged green, closed neutral", () => {
    const { card } = renderCard(mergeRequest({ state: "open" }));
    expect(card().getByText("Open").className).toContain("blue");
    cleanup();
    renderCard(mergeRequest({ state: "merged", closed_at: "2026-10-15T11:00:00Z" }));
    expect(screen.getByText("Merged").className).toContain("emerald");
    cleanup();
    renderCard(mergeRequest({ state: "closed", closed_at: "2026-10-15T11:00:00Z" }));
    expect(screen.getByText("Closed").className).toContain("zinc");
  });

  it("shows the time once the request is final, and none while it is open", () => {
    const closed_at = "2026-10-15T11:00:00Z";
    renderCard(mergeRequest({ state: "merged", closed_at }));
    expect(screen.getByText(formatDateTime(closed_at))).not.toBeNull();
    cleanup();
    renderCard(mergeRequest({ state: "open", closed_at: undefined, updated_at: "2026-10-15T10:02:00Z" }));
    expect(screen.queryByText(formatDateTime("2026-10-15T10:02:00Z"))).toBeNull();
  });
});

describe("review threads, sync line", () => {
  it.each<[number | null | undefined, string]>([
    [3, "3 unresolved threads"],
    [1, "1 unresolved thread"],
    [0, "No unresolved threads"],
    [null, "Threads unknown"],
    [undefined, "Threads unknown"],
  ])("says %s as %s", (unresolved_threads, text) => {
    renderCard(mergeRequest({ unresolved_threads }));
    expect(screen.getByText(text)).not.toBeNull();
  });

  it("shows the last sync error as a muted line", () => {
    renderCard(mergeRequest({ last_sync_error: "GraphQL access is missing for this token" }));
    const line = screen.getByText("GraphQL access is missing for this token");
    expect(line.className).toContain("zinc-500");
    expect(line.getAttribute("role")).toBeNull();
  });

  it("says when it last synced, and when it never did", () => {
    renderCard(mergeRequest({ synced_at: ago(5 * 60_000) }));
    expect(screen.getByText("Synced 5 min ago")).not.toBeNull();
    cleanup();
    api.on("POST", "/api/merge-requests/mr1/sync", () => new Promise(() => undefined));
    renderCard(mergeRequest({ synced_at: null }));
    expect(screen.getByText("Not synced yet")).not.toBeNull();
  });
});

describe("Merge", () => {
  it("is primary and enabled for an open request whose pipeline passed or does not exist", () => {
    renderCard(mergeRequest({ ci_status: "passed" }));
    expect(merge().disabled).toBe(false);
    expect(merge().className).toContain("bg-zinc-900");
    expect(merge().getAttribute("title")).toBeNull();
    cleanup();
    renderCard(mergeRequest({ ci_status: "none" }));
    expect(merge().disabled).toBe(false);
  });

  it.each<[string, Partial<MergeRequest>, string]>([
    ["pending", { ci_status: "pending" }, "The pipeline is still running."],
    ["failed", { ci_status: "failed" }, "The pipeline failed. Merge on GitLab to override."],
    ["failed on GitHub", { ci_status: "failed", provider: "github" }, "The pipeline failed. Merge on GitHub to override."],
  ])("is disabled, with a title, when the request is %s", (_, over, title) => {
    renderCard(mergeRequest(over));
    expect(merge().disabled).toBe(true);
    expect(merge().getAttribute("title")).toBe(title);
    fireEvent.click(merge());
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("asks first, naming the request, its title, the target branch and what happens", async () => {
    renderCard();
    fireEvent.click(merge());
    const d = await dialog();
    expect(d.getByRole("heading", { name: "Merge !12 into main?" })).not.toBeNull();
    expect(d.getByText("Add the health endpoint")).not.toBeNull();
    expect(d.getByText("The merge request is merged on GitLab and its source branch deleted.")).not.toBeNull();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(api.made("POST", /merge-requests/).filter((c) => c.url.endsWith("/merge"))).toHaveLength(0);
  });

  it("does not use the red button of a removal for the confirmation", async () => {
    renderCard();
    fireEvent.click(merge());
    const d = await dialog();
    const confirm = d.getByRole("button", { name: "Merge" });
    expect(confirm.className).toContain("bg-zinc-900");
    expect(confirm.className).not.toContain("bg-red");
  });

  it("says what a GitHub merge does without promising a deleted branch", async () => {
    renderCard(mergeRequest({ provider: "github", number: 3, target_branch: "develop" }));
    fireEvent.click(merge());
    const d = await dialog();
    expect(d.getByRole("heading", { name: "Merge #3 into develop?" })).not.toBeNull();
    expect(d.getByText("The pull request is merged on GitHub.")).not.toBeNull();
  });

  it("merges on confirmation, closes the dialog and hands over the record that came back", async () => {
    const merged = mergeRequest({ state: "merged", closed_at: "2026-10-15T11:00:00Z", merged_by: "operator" });
    api.on("POST", "/api/merge-requests/mr1/merge", () => merged);
    renderCard();
    fireEvent.click(merge());
    fireEvent.click((await dialog()).getByRole("button", { name: "Merge" }));
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(merged));
    expect(api.made("POST", "/api/merge-requests/mr1/merge")).toHaveLength(1);
    expect(api.made("POST", "/api/merge-requests/mr1/merge")[0].body).toBeUndefined();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("shows the server's message in the card on a 409, closes the dialog and reads the record again", async () => {
    api.on("POST", "/api/merge-requests/mr1/merge", () => apiError(409, "merge_request_not_mergeable", "Branch cannot be merged: conflicts"));
    const refreshed = mergeRequest({ ci_status: "failed" });
    api.on("POST", "/api/merge-requests/mr1/sync", () => refreshed);
    const { card } = renderCard();
    fireEvent.click(merge());
    fireEvent.click((await dialog()).getByRole("button", { name: "Merge" }));
    expect((await card().findByRole("alert")).textContent).toBe("Branch cannot be merged: conflicts");
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(refreshed));
    expect(card().getByRole("alert").textContent).toBe("Branch cannot be merged: conflicts");
  });

  it("clears the message when the merge is tried again", async () => {
    let attempts = 0;
    api.on("POST", "/api/merge-requests/mr1/merge", () => (++attempts === 1 ? apiError(409, "merge_request_not_open", "the request is closed") : mergeRequest({ state: "merged", closed_at: "2026-10-15T11:00:00Z" })));
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest());
    const { card } = renderCard();
    fireEvent.click(merge());
    fireEvent.click((await dialog()).getByRole("button", { name: "Merge" }));
    await card().findByRole("alert");
    fireEvent.click(merge());
    fireEvent.click((await dialog()).getByRole("button", { name: "Merge" }));
    await waitFor(() => expect(card().queryByRole("alert")).toBeNull());
  });
});

describe("a merged or closed request", () => {
  const closed_at = "2026-10-15T11:00:00Z";

  it.each<[MergeRequestState, string]>([
    ["merged", "Merged"],
    ["closed", "Closed"],
  ])("shows a %s request's state and when it ended, and offers neither Merge nor Sync", (state, label) => {
    const { card } = renderCard(mergeRequest({ state, closed_at }));
    expect(card().getByText(label)).not.toBeNull();
    expect(card().getByText(formatDateTime(closed_at))).not.toBeNull();
    expect(card().queryByRole("button", { name: "Merge" })).toBeNull();
    expect(card().queryByRole("button", { name: "Sync" })).toBeNull();
    expect(card().queryByRole("button")).toBeNull();
  });

  it("still links the request and names its branches", () => {
    const { card } = renderCard(mergeRequest({ state: "merged", closed_at }));
    expect(card().getByRole("link", { name: "Merge request !12" })).not.toBeNull();
    expect(card().getByText("yuno/0123456789ab → main")).not.toBeNull();
  });

  it("falls back to the time of the last update when the provider gave no end time", () => {
    const { card } = renderCard(mergeRequest({ state: "closed", closed_at: undefined, updated_at: "2026-10-15T10:30:00Z" }));
    expect(card().getByText(formatDateTime("2026-10-15T10:30:00Z"))).not.toBeNull();
    expect(card().queryByRole("button")).toBeNull();
  });

  it("loses the buttons when the record it shows turns merged, and an open Merge dialog with them", async () => {
    const merged = mergeRequest({ state: "merged", closed_at, merged_by: "operator" });
    const { rerender } = renderCard();
    fireEvent.click(merge());
    await dialog();
    rerender(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter>
          <MergeRequestCard mr={merged} onChange={onChange} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: "Merge" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Sync" })).toBeNull();
    expect(screen.getByText("Merged")).not.toBeNull();
  });

  it("keeps Merge and Sync on an open request", () => {
    const { card } = renderCard(mergeRequest({ state: "open" }));
    expect(card().getByRole("button", { name: "Merge" })).not.toBeNull();
    expect(card().getByRole("button", { name: "Sync" })).not.toBeNull();
  });
});

describe("Sync", () => {
  it("refreshes the record from the provider and hands over what came back", async () => {
    const fresh = mergeRequest({ ci_status: "pending", unresolved_threads: 2 });
    api.on("POST", "/api/merge-requests/mr1/sync", () => fresh);
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Sync" }));
    await waitFor(() => expect(onChange).toHaveBeenCalledWith(fresh));
    expect(api.made("POST", "/api/merge-requests/mr1/sync")).toHaveLength(1);
  });

  it("is busy while it runs", async () => {
    let release: (mr: MergeRequest) => void = () => undefined;
    api.on("POST", "/api/merge-requests/mr1/sync", () => new Promise<MergeRequest>((r) => (release = r)));
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Sync" }));
    const busy = (await screen.findByRole("button", { name: "Syncing…" })) as HTMLButtonElement;
    expect(busy.disabled).toBe(true);
    expect(merge().disabled).toBe(true);
    release(mergeRequest());
    await waitFor(() => expect((screen.getByRole("button", { name: "Sync" }) as HTMLButtonElement).disabled).toBe(false));
  });

  it("says why when the request itself fails", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => apiError(500, "internal", "boom"));
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Sync" }));
    expect((await screen.findByRole("alert")).textContent).toBe("Could not sync the merge request: boom");
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe("refreshing when the card appears", () => {
  const syncs = () => api.made("POST", "/api/merge-requests/mr1/sync");

  it("syncs an open record that was never synced", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest({ synced_at: ago(0) }));
    renderCard(mergeRequest({ synced_at: null }));
    await waitFor(() => expect(syncs()).toHaveLength(1));
    await waitFor(() => expect(onChange).toHaveBeenCalledTimes(1));
  });

  it("syncs an open record last synced more than 30 s ago", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest());
    renderCard(mergeRequest({ synced_at: ago(31_000) }));
    await waitFor(() => expect(syncs()).toHaveLength(1));
  });

  it("leaves a record synced within 30 s alone", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest());
    renderCard(mergeRequest({ synced_at: ago(20_000) }));
    await new Promise((r) => setTimeout(r, 30));
    expect(syncs()).toHaveLength(0);
  });

  it("does not ask the provider about a request that is already merged or closed", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest());
    renderCard(mergeRequest({ state: "merged", closed_at: "2026-10-15T11:00:00Z", synced_at: null }));
    await new Promise((r) => setTimeout(r, 30));
    expect(syncs()).toHaveLength(0);
  });

  it("does not sync again when the record it gets back replaces the one shown", async () => {
    api.on("POST", "/api/merge-requests/mr1/sync", () => mergeRequest({ synced_at: ago(0) }));
    const { rerender } = renderCard(mergeRequest({ synced_at: null }));
    await waitFor(() => expect(syncs()).toHaveLength(1));
    const client = new QueryClient();
    rerender(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <MergeRequestCard mr={mergeRequest({ synced_at: null, ci_status: "pending" })} onChange={onChange} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await new Promise((r) => setTimeout(r, 30));
    expect(syncs()).toHaveLength(1);
  });
});
