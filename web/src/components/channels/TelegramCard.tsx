import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { ApiError, bindTelegram, listAgents, unbindTelegram, type ChannelsInfo } from "@/lib/api";
import { channelsKey, useChannels } from "@/lib/useChannels";
import { Badge } from "../Badge";
import Button from "../Button";
import { Card } from "../Card";
import { control } from "../fields";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";

const MODE_LABEL: Record<string, string> = { polling: "Polling", webhook: "Webhook", off: "Off" };

/** The Telegram channel: whether it is on, who may write to the bot, and which agent answers. */
export default function TelegramCard() {
  const qc = useQueryClient();
  const channels = useChannels();
  const agents = useQuery({ queryKey: ["agents"], queryFn: listAgents });
  const [pick, setPick] = useState("");
  const [conflict, setConflict] = useState(false);

  const bind = useMutation({
    mutationFn: (agentId: string) => bindTelegram(agentId),
    onSuccess: (info) => {
      setConflict(false);
      setPick("");
      qc.setQueryData(channelsKey, info);
      void qc.invalidateQueries({ queryKey: ["agents"] });
      void qc.invalidateQueries({ queryKey: ["agent"] });
    },
    onError: (err) => {
      // Someone bound an agent after this page loaded: show what is bound now.
      if (err instanceof ApiError && err.status === 409) {
        setConflict(true);
        void qc.invalidateQueries({ queryKey: channelsKey });
      }
    },
  });

  const unbind = useMutation({
    mutationFn: unbindTelegram,
    onSuccess: () => {
      setConflict(false);
      qc.setQueryData<ChannelsInfo>(channelsKey, (old) => (old ? { telegram: { ...old.telegram, binding: null } } : old));
      void qc.invalidateQueries({ queryKey: channelsKey });
      void qc.invalidateQueries({ queryKey: ["agent"] });
    },
  });

  const t = channels.data?.telegram;
  const bindFailed = bind.error && !(bind.error instanceof ApiError && bind.error.status === 409);

  return (
    <Card>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">Telegram</h2>
        {t ? (
          <>
            <Badge tone={t.enabled ? "green" : "zinc"}>{t.enabled ? "Enabled" : "Disabled"}</Badge>
            {t.enabled ? <Badge tone="zinc">{MODE_LABEL[t.mode] ?? t.mode}</Badge> : null}
          </>
        ) : null}
      </div>

      {channels.isLoading ? <Loading label="Loading Telegram…" /> : null}
      {channels.error ? <QueryError what="the Telegram channel" error={channels.error} onRetry={() => void channels.refetch()} /> : null}

      {t ? (
        <dl className="grid gap-x-6 gap-y-4 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-xs uppercase tracking-wide text-zinc-500">Bot</dt>
            <dd className="mt-1 font-medium">{t.bot_username ? `@${t.bot_username.replace(/^@/, "")}` : "—"}</dd>
          </div>
          <div>
            <dt className="text-xs uppercase tracking-wide text-zinc-500">Allowed users</dt>
            <dd className="mt-1">
              {t.allowed_user_ids.length > 0 ? (
                <ul className="flex flex-wrap gap-1.5">
                  {t.allowed_user_ids.map((id) => (
                    <li key={id} className="rounded-full bg-zinc-100 px-2.5 py-0.5 font-mono text-xs dark:bg-zinc-800">
                      {id}
                    </li>
                  ))}
                </ul>
              ) : (
                <span className="text-zinc-500">Nobody is allowed to write to the bot.</span>
              )}
              <p className="mt-1.5 text-xs text-zinc-500">Messages from anyone else are ignored.</p>
            </dd>
          </div>

          <div className="sm:col-span-2">
            <dt className="text-xs uppercase tracking-wide text-zinc-500">Agent</dt>
            <dd className="mt-1">
              {t.binding ? (
                <div className="flex flex-wrap items-center gap-3">
                  <span>
                    Messages are answered by{" "}
                    <Link to={`/agents/${t.binding.agent_id}`} className="font-medium hover:underline">
                      {t.binding.agent_name}
                    </Link>
                    .
                  </span>
                  <Button size="sm" onClick={() => unbind.mutate()} disabled={unbind.isPending} aria-label={`Unbind ${t.binding.agent_name}`}>
                    {unbind.isPending ? "Unbinding…" : "Unbind"}
                  </Button>
                </div>
              ) : (
                <form
                  className="space-y-2"
                  onSubmit={(e) => {
                    e.preventDefault();
                    if (pick && !bind.isPending) bind.mutate(pick);
                  }}
                >
                  <p className="text-zinc-600 dark:text-zinc-400">No agent is bound, so messages sent to the bot are not answered.</p>
                  <div className="flex flex-wrap items-center gap-2">
                    <select aria-label="Agent to bind" value={pick} onChange={(e) => setPick(e.target.value)} className={control(false, "!w-64 max-w-full")}>
                      <option value="">Choose an agent</option>
                      {(agents.data ?? []).map((a) => (
                        <option key={a.id} value={a.id}>
                          {a.name}
                        </option>
                      ))}
                    </select>
                    <Button type="submit" variant="primary" disabled={!pick || bind.isPending}>
                      {bind.isPending ? "Binding…" : "Bind"}
                    </Button>
                  </div>
                  {agents.error ? <InlineError>Could not load agents: {errorText(agents.error)}</InlineError> : null}
                </form>
              )}
              {conflict ? <p role="alert" className="mt-2 text-sm text-amber-700 dark:text-amber-400">Another agent is bound to Telegram. Unbind it before binding a different one.</p> : null}
              {bindFailed ? <div className="mt-2"><InlineError>Could not bind the agent: {errorText(bind.error)}</InlineError></div> : null}
              {unbind.error ? <div className="mt-2"><InlineError>Could not unbind the agent: {errorText(unbind.error)}</InlineError></div> : null}
            </dd>
          </div>
        </dl>
      ) : null}
    </Card>
  );
}
