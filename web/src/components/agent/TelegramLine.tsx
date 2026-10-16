import { Link } from "react-router";
import { useChannels } from "@/lib/useChannels";
import { Badge } from "../Badge";

/** Whether this agent answers Telegram, with the way to change it. Binding happens on the Channels page. */
export default function TelegramLine({ agentId }: { agentId: string }) {
  const q = useChannels();
  const binding = q.data?.telegram.binding;
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
      <span className="font-medium">Telegram</span>
      {q.data ? (
        binding?.agent_id === agentId ? (
          <Badge tone="green">Bound to this agent</Badge>
        ) : binding ? (
          <Badge tone="zinc">Bound to {binding.agent_name}</Badge>
        ) : (
          <Badge tone="amber">Not bound</Badge>
        )
      ) : null}
      <Link to="/channels" className="underline">
        Manage in Channels
      </Link>
    </div>
  );
}
