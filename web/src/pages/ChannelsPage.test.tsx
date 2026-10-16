// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { Agent, ChannelsInfo, Conversation, ConversationMessage } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { apiError, installFakeApi } from "@/test/fakeApi";
import ChannelsPage from "./ChannelsPage";

const RUN = "01a14161-d5ef-7d96-ba96-a45d5d4bb0f2";

const agent = (id: string, name: string): Agent => ({ id, name, model: "claude-opus-5-5", created_at: "2026-10-15T09:00:00Z", updated_at: "2026-10-15T09:00:00Z" });
const channels = (over: Partial<ChannelsInfo["telegram"]> = {}): ChannelsInfo => ({
  telegram: { enabled: true, mode: "polling", bot_username: "yuno_bot", allowed_user_ids: [1001, 1002], binding: null, ...over },
});
const conv = (over: Partial<Conversation>): Conversation => ({
  id: "c1",
  channel: "telegram",
  external_chat_id: "1001",
  agent_id: "a1",
  agent_name: "Coder",
  last_message_at: "2026-10-15T10:05:00Z",
  message_count: 2,
  ...over,
});
const msg = (over: Partial<ConversationMessage>): ConversationMessage => ({ id: "m1", direction: "in", text: "hello", created_at: "2026-10-15T10:00:00Z", ...over });

let info: ChannelsInfo;
let conversations: Conversation[];
let messages: Record<string, ConversationMessage[]>;
let api: ReturnType<typeof installFakeApi>;

beforeEach(() => {
  info = channels();
  conversations = [
    conv({}),
    conv({ id: "c2", external_chat_id: "1002", display_name: "Maria", agent_id: "a2", agent_name: "Reviewer", last_message_at: "2026-10-15T11:00:00Z", message_count: 1 }),
  ];
  messages = {
    c1: [
      msg({ id: "m2", direction: "out", text: "Sure, deploying now", run_id: RUN, created_at: "2026-10-15T10:05:00Z" }),
      msg({ id: "m1", direction: "in", text: "Please deploy 1.2.0", created_at: "2026-10-15T10:00:00Z" }),
    ],
    c2: [msg({ id: "m3", text: "What changed?", created_at: "2026-10-15T11:00:00Z" })],
  };
  api = installFakeApi();
  api.on("GET", "/api/channels", () => info);
  api.on("GET", "/api/agents", () => [agent("a1", "Coder"), agent("a2", "Reviewer")]);
  api.on("GET", "/api/conversations", () => conversations);
  api.on("GET", /^\/api\/conversations\/([^/]+)\/messages$/, (r) => messages[r.match[1]] ?? []);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage(path = "/channels") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/channels" element={<ChannelsPage />} />
          <Route path="/channels/:conversationId" element={<ChannelsPage />} />
          <Route path="/agents/:id" element={<p>agent page</p>} />
          <Route path="/runs/:id" element={<p>run page</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const telegramCard = async () => within((await screen.findByRole("heading", { name: "Telegram" })).closest("div.rounded-lg") as HTMLElement);

describe("Telegram card", () => {
  it("shows whether it is on, the bot and who may write to it", async () => {
    renderPage();
    const card = await telegramCard();
    expect(await card.findByText("Enabled")).not.toBeNull();
    expect(card.getByText("Polling")).not.toBeNull();
    expect(card.getByText("@yuno_bot")).not.toBeNull();
    expect(card.getByText("1001")).not.toBeNull();
    expect(card.getByText("1002")).not.toBeNull();
    expect(card.getByText(/not answered/)).not.toBeNull();
  });

  it("says when it is off and when nobody is allowed", async () => {
    info = channels({ enabled: false, mode: "off", bot_username: undefined, allowed_user_ids: [] });
    renderPage();
    const card = await telegramCard();
    expect(await card.findByText("Disabled")).not.toBeNull();
    expect(card.queryByText("Off")).toBeNull();
    expect(card.getByText("Nobody is allowed to write to the bot.")).not.toBeNull();
  });

  it("binds the chosen agent and then shows it with a way to unbind", async () => {
    api.on("PUT", "/api/channels/telegram/binding", (r) => {
      const id = (r.body as { agent_id: string }).agent_id;
      info = channels({ binding: { agent_id: id, agent_name: id === "a2" ? "Reviewer" : "Coder" } });
      return info;
    });
    renderPage();
    const card = await telegramCard();
    const bind = card.getByRole("button", { name: "Bind" }) as HTMLButtonElement;
    expect(bind.disabled).toBe(true);
    await waitFor(() => expect(card.getByRole("option", { name: "Reviewer" })).not.toBeNull());
    fireEvent.change(card.getByLabelText("Agent to bind"), { target: { value: "a2" } });
    fireEvent.click(bind);

    const link = await card.findByRole("link", { name: "Reviewer" });
    expect(link.getAttribute("href")).toBe("/agents/a2");
    expect(api.made("PUT", "/api/channels/telegram/binding")[0].body).toEqual({ agent_id: "a2" });
    expect(card.queryByLabelText("Agent to bind")).toBeNull();
    expect(card.getByRole("button", { name: "Unbind Reviewer" })).not.toBeNull();
  });

  it("explains a refusal when another agent got bound first, and shows who is bound", async () => {
    api.on("PUT", "/api/channels/telegram/binding", () => {
      info = channels({ binding: { agent_id: "a1", agent_name: "Coder" } });
      return apiError(409, "conflict", "telegram already bound to agent a1");
    });
    renderPage();
    const card = await telegramCard();
    await waitFor(() => expect(card.getByRole("option", { name: "Reviewer" })).not.toBeNull());
    fireEvent.change(card.getByLabelText("Agent to bind"), { target: { value: "a2" } });
    fireEvent.click(card.getByRole("button", { name: "Bind" }));

    expect((await card.findByRole("alert")).textContent).toContain("Another agent is bound");
    expect(await card.findByRole("link", { name: "Coder" })).not.toBeNull();
    expect(card.queryByText(/telegram already bound/)).toBeNull();
  });

  it("unbinds the agent and offers the picker again", async () => {
    info = channels({ binding: { agent_id: "a1", agent_name: "Coder" } });
    api.on("DELETE", "/api/channels/telegram/binding", () => {
      info = channels();
      return new Response(null, { status: 204 });
    });
    renderPage();
    const card = await telegramCard();
    fireEvent.click(await card.findByRole("button", { name: "Unbind Coder" }));
    expect(await card.findByLabelText("Agent to bind")).not.toBeNull();
    expect(api.made("DELETE", "/api/channels/telegram/binding")).toHaveLength(1);
  });

  it("reports an unbind that failed", async () => {
    info = channels({ binding: { agent_id: "a1", agent_name: "Coder" } });
    api.on("DELETE", "/api/channels/telegram/binding", () => apiError(502, "gateway_error", "gateway unavailable"));
    renderPage();
    const card = await telegramCard();
    fireEvent.click(await card.findByRole("button", { name: "Unbind Coder" }));
    expect((await card.findByRole("alert")).textContent).toContain("gateway unavailable");
    expect(card.getByRole("link", { name: "Coder" })).not.toBeNull();
  });
});

describe("Conversations", () => {
  it("lists each with agent, chat, last message and count, newest activity first", async () => {
    renderPage();
    const first = await screen.findByRole("link", { name: /Reviewer/ });
    const items = screen.getAllByRole("listitem").filter((li) => li.querySelector("a[href^='/channels/']"));
    expect(items.map((li) => li.querySelector("a")!.getAttribute("href"))).toEqual(["/channels/c2", "/channels/c1"]);
    expect(first.textContent).toContain("Maria");
    expect(first.textContent).toContain("1 message");
    expect(first.textContent).toContain(`Last message ${formatDateTime("2026-10-15T11:00:00Z")}`);
    const other = screen.getByRole("link", { name: /Coder/ });
    expect(other.textContent).toContain("1001");
    expect(other.textContent).toContain("2 messages");
  });

  it("says so when there are none", async () => {
    conversations = [];
    renderPage();
    expect(await screen.findByText(/No conversations yet/)).not.toBeNull();
  });

  it("opens the transcript with received and sent messages in time order and a link to the run", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("link", { name: /Coder/ }));
    const transcript = within(await screen.findByRole("region", { name: "Conversation with Coder" }));
    const bubbles = await waitFor(() => {
      const found = transcript.getAllByRole("listitem");
      expect(found).toHaveLength(2);
      return found;
    });
    expect(bubbles.map((b) => b.getAttribute("data-direction"))).toEqual(["in", "out"]);
    expect(bubbles[0].textContent).toContain("Please deploy 1.2.0");
    expect(bubbles[0].textContent).toContain(formatDateTime("2026-10-15T10:00:00Z"));
    expect(bubbles[1].textContent).toContain("Sure, deploying now");
    expect(within(bubbles[0]).queryByRole("link")).toBeNull();
    const run = within(bubbles[1]).getByRole("link", { name: `Run ${shortId(RUN)}` });
    expect(run.getAttribute("href")).toBe(`/runs/${RUN}`);
    expect(screen.getByRole("link", { name: /Coder/ }).getAttribute("aria-current")).toBe("true");
  });

  it("opens straight on a conversation from its address", async () => {
    renderPage("/channels/c2");
    const transcript = within(await screen.findByRole("region", { name: "Conversation with Reviewer" }));
    expect(await transcript.findByText("What changed?")).not.toBeNull();
    expect(transcript.getByRole("heading", { name: "Maria" })).not.toBeNull();
  });

  it("says when the conversation in the address no longer exists", async () => {
    renderPage("/channels/gone");
    expect(await screen.findByText("This conversation no longer exists.")).not.toBeNull();
  });

  it("appends a new message to the open transcript as it arrives, and moves the row up with its new count", async () => {
    renderPage("/channels/c1");
    const transcript = within(await screen.findByRole("region", { name: "Conversation with Coder" }));
    await transcript.findByText("Please deploy 1.2.0");
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));

    const m = msg({ id: "m9", direction: "in", text: "And tag it", created_at: "2026-10-15T12:00:00Z" });
    api.send("/api/events", "conversation.message", { conversation: conv({ last_message_at: m.created_at, message_count: 3 }), message: m });
    expect(await transcript.findByText("And tag it")).not.toBeNull();
    await waitFor(() => expect(screen.getByRole("link", { name: /Coder/ }).textContent).toContain("3 messages"));
    const hrefs = screen.getAllByRole("link").map((l) => l.getAttribute("href")).filter((h) => h?.startsWith("/channels/"));
    expect(hrefs).toEqual(["/channels/c1", "/channels/c2"]);

    // The same message delivered twice is shown once.
    api.send("/api/events", "conversation.message", { conversation: conv({ last_message_at: m.created_at, message_count: 3 }), message: m });
    await new Promise((r) => setTimeout(r, 20));
    expect(transcript.getAllByText("And tag it")).toHaveLength(1);
  });

  it("updates another conversation's row without opening it", async () => {
    renderPage("/channels/c1");
    await screen.findByRole("region", { name: "Conversation with Coder" });
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    api.send("/api/events", "conversation.message", {
      conversation: conv({ id: "c2", agent_id: "a2", agent_name: "Reviewer", last_message_at: "2026-10-15T12:30:00Z", message_count: 4 }),
      message: msg({ id: "m10", text: "ping", created_at: "2026-10-15T12:30:00Z" }),
    });
    await waitFor(() => expect(screen.getByRole("link", { name: /Reviewer/ }).textContent).toContain("4 messages"));
    expect(screen.queryByText("ping")).toBeNull();
  });

  it("reloads the list when a conversation it does not know starts", async () => {
    renderPage();
    await screen.findByRole("link", { name: /Coder/ });
    await waitFor(() => expect(api.hasStream("/api/events")).toBe(true));
    const fresh = conv({ id: "c3", agent_id: "a2", agent_name: "Reviewer", external_chat_id: "1003", message_count: 1, last_message_at: "2026-10-15T13:00:00Z" });
    conversations = [...conversations, fresh];
    api.send("/api/events", "conversation.message", { conversation: fresh, message: msg({ id: "m11", created_at: fresh.last_message_at }) });
    await waitFor(() => expect(screen.getAllByRole("link", { name: /Reviewer/ })).toHaveLength(2));
  });
});
