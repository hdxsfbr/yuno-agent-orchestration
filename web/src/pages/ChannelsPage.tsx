import { useParams } from "react-router";
import { useConversationEvents, useConversations } from "@/lib/useChannels";
import ConversationList from "@/components/channels/ConversationList";
import TelegramCard from "@/components/channels/TelegramCard";
import ToolCalls from "@/components/channels/ToolCalls";
import Transcript from "@/components/channels/Transcript";
import { PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { EmptyState, Loading, QueryError } from "@/components/QueryState";

export default function ChannelsPage() {
  const { conversationId } = useParams();
  const connection = useConversationEvents();
  const q = useConversations();
  const selected = q.data?.find((c) => c.id === conversationId);

  return (
    <div className="max-w-5xl space-y-6">
      <PageHeader title="Channels">
        <ConnectionIndicator state={connection} />
      </PageHeader>

      <TelegramCard />

      <section aria-labelledby="conversations-heading">
        <h2 id="conversations-heading" className="mb-3 text-base font-semibold">
          Conversations
        </h2>
        {q.isLoading ? <Loading label="Loading conversations…" /> : null}
        {q.error ? <QueryError what="conversations" error={q.error} onRetry={() => void q.refetch()} /> : null}
        {q.data && q.data.length === 0 ? <EmptyState>No conversations yet. Messages people send to a bound agent appear here.</EmptyState> : null}
        {q.data && q.data.length > 0 ? (
          <div className="grid gap-4 lg:grid-cols-[20rem_minmax(0,1fr)]">
            <ConversationList conversations={q.data} selectedId={conversationId} />
            {selected ? (
              <div className="min-w-0 space-y-4">
                <Transcript key={selected.id} conversation={selected} />
                <ToolCalls conversationId={selected.id} />
              </div>
            ) : (
              <p className="rounded-lg border border-dashed border-zinc-300 p-6 text-center text-sm text-zinc-500 dark:border-zinc-700">
                {conversationId ? "This conversation no longer exists." : "Choose a conversation to read it."}
              </p>
            )}
          </div>
        ) : null}
      </section>
    </div>
  );
}
