// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError, type Run } from "@/lib/api";
import { apiError, installFakeApi } from "@/test/fakeApi";
import { run } from "@/test/fixtures";
import RetryDialog from "./RetryDialog";

let api: ReturnType<typeof installFakeApi>;
let onClose: ReturnType<typeof vi.fn<() => void>>;
let onRetried: ReturnType<typeof vi.fn<(run: Run) => void>>;

beforeEach(() => {
  api = installFakeApi();
  onClose = vi.fn<() => void>();
  onRetried = vi.fn<(run: Run) => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const exhausted = (details: unknown) => new ApiError(409, "limits_exhausted", "the run used up its limits", details);
const stepsUsed = [{ path: "max_steps", message: "40 of 40 used" }];

function renderDialog(over: { run?: Run; refusal?: ApiError; repeatsSideEffects?: boolean } = {}) {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RetryDialog run={over.run ?? run({ limits: { max_steps: 40, max_tool_calls: 60, max_cost_usd: 2 } })} refusal={over.refusal ?? exhausted(stepsUsed)} repeatsSideEffects={over.repeatsSideEffects} onClose={onClose} onRetried={onRetried} />
    </QueryClientProvider>,
  );
  return within(screen.getByRole("dialog"));
}

const field = (d: ReturnType<typeof within>, label: RegExp) => d.getByLabelText(label) as HTMLInputElement;
const retry = (d: ReturnType<typeof within>) => fireEvent.click(d.getByRole("button", { name: "Retry" }));
const sent = () => api.made("POST", "/api/runs/r1/retry");

describe("what the dialog shows", () => {
  it("lists each exhausted limit with the server's sentence and says what to do", () => {
    const d = renderDialog({
      refusal: exhausted([
        { path: "max_steps", message: "40 of 40 used" },
        { path: "max_tool_calls", message: "61 of 60 used" },
      ]),
    });
    expect(d.getByRole("heading", { name: "Retry run" })).not.toBeNull();
    const items = d.getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual(["Max steps: 40 of 40 used", "Max tool calls: 61 of 60 used"]);
    expect(d.getByText("Raise the limits that are used up to retry from where the run stopped. A field left empty keeps its current value.")).not.toBeNull();
  });

  it("words each limit once when the API repeats its name in the sentence", () => {
    const d = renderDialog({ refusal: exhausted([{ path: "max_steps", message: "max_steps: 40 of 40 used" }]) });
    expect(d.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Max steps: 40 of 40 used"]);
  });

  it("reads details sent as `<limit>: <sentence>` lines", () => {
    const d = renderDialog({ refusal: exhausted(["max_cost_usd: $2.10 of $2.00 used"]) });
    expect(d.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Max cost (USD): $2.10 of $2.00 used"]);
  });

  it("fills the three counted limits from the run's limits, and marks only the used up ones as required", () => {
    const d = renderDialog();
    expect(field(d, /^Max steps/).value).toBe("40");
    expect(field(d, /^Max tool calls/).value).toBe("60");
    expect(field(d, /^Max cost \(USD\)/).value).toBe("2");
    expect(field(d, /^Max steps/).labels?.[0].textContent).toContain("*");
    expect(field(d, /^Max tool calls/).labels?.[0].textContent).not.toContain("*");
    expect(field(d, /^Max cost/).labels?.[0].textContent).not.toContain("*");
    expect(d.queryByLabelText(/Max duration/)).toBeNull();
    expect(d.queryByLabelText(/Max loop visits/)).toBeNull();
  });

  it("says an empty field keeps its current value, in the hint and in the placeholder, not the platform default", () => {
    const d = renderDialog({ run: run({ limits: {} }) });
    expect(d.getByText(/A field left empty keeps its current value\./)).not.toBeNull();
    expect(field(d, /^Max tool calls/).value).toBe("");
    expect(field(d, /^Max tool calls/).placeholder).toBe("Unchanged");
    expect(field(d, /^Max cost/).placeholder).toBe("Unchanged");
    expect(d.queryByText(/platform default/i)).toBeNull();
  });

  it("shows the loop visits of the step as a fourth field when they are what is used up", () => {
    const d = renderDialog({
      run: run({ limits: { max_steps: 40, max_tool_calls: 60, max_cost_usd: 2, max_loop_visits: 3 } }),
      refusal: exhausted([{ path: "max_loop_visits", message: "max_loop_visits: 3 of 3 used" }]),
    });
    expect(d.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Max loop visits: 3 of 3 used"]);
    const labels = d.getAllByRole("textbox").map((input) => (input as HTMLInputElement).labels?.[0].textContent);
    expect(labels).toEqual(["Max steps", "Max cost (USD)", "Max tool calls", "Max loop visits*"]);
    expect(field(d, /^Max loop visits/).value).toBe("3");
    expect(field(d, /^Max loop visits/).labels?.[0].textContent).toContain("*");
    expect(field(d, /^Max steps/).labels?.[0].textContent).not.toContain("*");
  });

  it("starts the loop visits from the cap of the sentence when the run carries no limit of its own", () => {
    const d = renderDialog({ run: run({ limits: {} }), refusal: exhausted([{ path: "max_loop_visits", message: "3 of 3 used" }]) });
    expect(field(d, /^Max loop visits/).value).toBe("3");
  });

  it("starts an exhausted limit from the cap the sentence names when the run carries no limits of its own", () => {
    const d = renderDialog({ run: run({ limits: {} }), refusal: exhausted([{ path: "max_cost_usd", message: "$2.10 of $2.00 used" }]) });
    expect(field(d, /^Max cost/).value).toBe("2.00");
    expect(field(d, /^Max steps/).value).toBe("");
  });

  it("says nothing about repeated steps unless the run already deployed or created something", () => {
    expect(renderDialog().queryByText(/already deployed a release/)).toBeNull();
    cleanup();
    expect(renderDialog({ repeatsSideEffects: true }).getByText(/already deployed a release or created a merge request through the GitLab API/)).not.toBeNull();
  });
});

describe("raising the limits", () => {
  it("sends the raised limit with everything else the run already had", async () => {
    api.on("POST", "/api/runs/r1/retry", () => run({ status: "queued", retries: 1 }));
    const d = renderDialog({ run: run({ limits: { max_steps: 40, max_tool_calls: 60, max_cost_usd: 2, max_duration_seconds: 900, max_loop_visits: 3 } }) });
    fireEvent.change(field(d, /^Max steps/), { target: { value: " 80 " } });
    retry(d);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0].body).toEqual({ limits: { max_steps: 80, max_tool_calls: 60, max_cost_usd: 2, max_duration_seconds: 900, max_loop_visits: 3 } });
    await waitFor(() => expect(onRetried).toHaveBeenCalledWith(expect.objectContaining({ status: "queued", retries: 1 })));
    expect(onClose).toHaveBeenCalled();
  });

  it("sends the loop visits raised, with the limits the run already had", async () => {
    api.on("POST", "/api/runs/r1/retry", () => run({ status: "queued", retries: 1 }));
    const d = renderDialog({
      run: run({ limits: { max_steps: 40, max_cost_usd: 2, max_loop_visits: 3 } }),
      refusal: exhausted([{ path: "max_loop_visits", message: "max_loop_visits: 3 of 3 used" }]),
    });
    fireEvent.change(field(d, /^Max loop visits/), { target: { value: "5" } });
    retry(d);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0].body).toEqual({ limits: { max_steps: 40, max_cost_usd: 2, max_loop_visits: 5 } });
    await waitFor(() => expect(onRetried).toHaveBeenCalledWith(expect.objectContaining({ status: "queued", retries: 1 })));
  });

  it("will not send loop visits that were left as they were", async () => {
    const d = renderDialog({
      run: run({ limits: { max_steps: 40, max_loop_visits: 3 } }),
      refusal: exhausted([{ path: "max_loop_visits", message: "3 of 3 used" }]),
    });
    retry(d);
    await waitFor(() => expect(field(d, /^Max loop visits/).parentElement?.textContent).toContain("Max loop visits is used up: raise it above 3"));
    expect(sent()).toHaveLength(0);
  });

  it("leaves a field that was emptied out of the body, so the run keeps its value for it", async () => {
    api.on("POST", "/api/runs/r1/retry", () => run({ status: "queued" }));
    const d = renderDialog({ run: run({ limits: { max_steps: 40, max_tool_calls: 60, max_cost_usd: 2, max_loop_visits: 3 } }) });
    fireEvent.change(field(d, /^Max steps/), { target: { value: "80" } });
    fireEvent.change(field(d, /^Max cost/), { target: { value: "" } });
    retry(d);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0].body).toEqual({ limits: { max_steps: 80, max_tool_calls: 60, max_loop_visits: 3 } });
  });

  it("lets a limit that is not used up change too, and a decimal cost", async () => {
    api.on("POST", "/api/runs/r1/retry", () => run({ status: "queued" }));
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "41" } });
    fireEvent.change(field(d, /^Max cost/), { target: { value: "3.25" } });
    fireEvent.change(field(d, /^Max tool calls/), { target: { value: "" } });
    retry(d);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0].body).toEqual({ limits: { max_steps: 41, max_cost_usd: 3.25 } });
  });

  it("will not send a used up limit that was left as it was, or lowered, or emptied", async () => {
    const d = renderDialog();
    retry(d);
    await waitFor(() => expect(field(d, /^Max steps/).getAttribute("aria-invalid")).toBe("true"));
    expect(field(d, /^Max steps/).parentElement?.textContent).toContain("Max steps is used up: raise it above 40");
    fireEvent.change(field(d, /^Max steps/), { target: { value: "30" } });
    retry(d);
    await waitFor(() => expect(field(d, /^Max steps/).parentElement?.textContent).toContain("raise it above 40"));
    fireEvent.change(field(d, /^Max steps/), { target: { value: "" } });
    retry(d);
    await waitFor(() => expect(field(d, /^Max steps/).parentElement?.textContent).toContain("enter a higher limit"));
    expect(sent()).toHaveLength(0);
  });

  it("will not send text that is not a number, or a fraction of a step", async () => {
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "4.5" } });
    fireEvent.change(field(d, /^Max cost/), { target: { value: "lots" } });
    retry(d);
    await waitFor(() => expect(field(d, /^Max steps/).parentElement?.textContent).toContain("Max steps must be a whole number"));
    expect(field(d, /^Max cost/).parentElement?.textContent).toContain("Max cost (USD) must be a number");
    expect(sent()).toHaveLength(0);
  });
});

describe("when the retry is refused again", () => {
  it("shows the limits that are still used up, from the new refusal, and stays open", async () => {
    api.on("POST", "/api/runs/r1/retry", () =>
      apiError(409, "limits_exhausted", "the run used up its limits", [
        { path: "max_steps", message: "41 of 41 used" },
        { path: "max_cost_usd", message: "$2.10 of $2.00 used" },
      ]),
    );
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "41" } });
    retry(d);
    await waitFor(() => expect(d.getAllByRole("listitem")).toHaveLength(2));
    expect(d.getAllByRole("listitem")[0].textContent).toBe("Max steps: 41 of 41 used");
    expect(d.getByRole("alert").textContent).toBe("the run used up its limits");
    expect(onClose).not.toHaveBeenCalled();
    expect(onRetried).not.toHaveBeenCalled();
    expect(field(d, /^Max cost/).labels?.[0].textContent).toContain("*");
  });

  it("shows the message of any other refusal inline", async () => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(409, "retry_limit", "run was retried 5 times"));
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "80" } });
    retry(d);
    expect((await d.findByRole("alert")).textContent).toBe("run was retried 5 times");
    expect(screen.getByRole("dialog")).not.toBeNull();
  });

  it("puts a 422 next to the limit it names", async () => {
    api.on("POST", "/api/runs/r1/retry", () => apiError(422, "validation_failed", "validation failed", [{ path: "limits.max_cost_usd", message: "must be at most 50" }]));
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "80" } });
    fireEvent.change(field(d, /^Max cost/), { target: { value: "500" } });
    retry(d);
    await waitFor(() => expect(field(d, /^Max cost/).parentElement?.textContent).toContain("must be at most 50"));
  });
});

describe("leaving", () => {
  it("closes on Cancel without a request", () => {
    const d = renderDialog();
    fireEvent.click(d.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(sent()).toHaveLength(0);
  });

  it("is busy while the request is out", async () => {
    let release: (r: Run) => void = () => undefined;
    api.on("POST", "/api/runs/r1/retry", () => new Promise<Run>((r) => (release = r)));
    const d = renderDialog();
    fireEvent.change(field(d, /^Max steps/), { target: { value: "80" } });
    retry(d);
    const busy = (await d.findByRole("button", { name: "Retrying…" })) as HTMLButtonElement;
    expect(busy.disabled).toBe(true);
    expect((d.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
    release(run({ status: "queued" }));
    await waitFor(() => expect(onRetried).toHaveBeenCalled());
  });
});
