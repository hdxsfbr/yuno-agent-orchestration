import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "react-router";
import { ApiError, createAgent, deleteAgent, getAgent, listAgents, listTools, materializeAgent, updateAgent, type Agent, type AgentInput } from "@/lib/api";
import { parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { formatDateTime } from "@/lib/format";
import { useSkills } from "@/lib/useSkills";
import AgentForm from "@/components/agent/AgentForm";
import AgentFiles from "@/components/agent/AgentFiles";
import MemoryFacts from "@/components/agent/MemoryFacts";
import RuntimeCard from "@/components/agent/RuntimeCard";
import TelegramLine from "@/components/agent/TelegramLine";
import Button from "@/components/Button";
import { PageHeader } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import QuickRunDialog from "@/components/QuickRunDialog";
import { Loading, QueryError, errorText } from "@/components/QueryState";

/** State handed over by navigation after a create, so the next page can say what happened. */
type NavState = { notice?: string; materializeFailed?: string } | null;

/** Where the API may put the id of an agent it saved but could not materialise. */
function agentIdFrom(details: unknown): string | undefined {
  const d = details as { id?: unknown; agent_id?: unknown; agent?: { id?: unknown } } | null | undefined;
  for (const v of [d?.agent_id, d?.id, d?.agent?.id]) if (typeof v === "string" && v) return v;
  return undefined;
}

/** The newest agent with this name: the one a create that answered 502 just stored. */
async function findSavedAgent(name: string): Promise<Agent | undefined> {
  const all = await listAgents();
  return all.filter((a) => a.name === name).sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
}

export default function AgentFormPage() {
  const { id } = useParams();
  return id ? <EditAgent key={id} id={id} /> : <CreateAgent />;
}

function useToolList() {
  return useQuery({ queryKey: ["tools"], queryFn: listTools, staleTime: 60_000 });
}

function CreateAgent() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const tools = useToolList();
  const skills = useSkills();
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (input: AgentInput) => createAgent(input),
    onSuccess: (agent) => {
      void qc.invalidateQueries({ queryKey: ["agents"] });
      qc.setQueryData(["agent", agent.id], agent);
      navigate(`/agents/${agent.id}`, { replace: true, state: { notice: "Agent created." } satisfies NavState });
    },
    onError: async (err, input) => {
      void qc.invalidateQueries({ queryKey: ["agents"] });
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
      } else if (err instanceof ApiError && err.status === 502) {
        // Saved, but OpenClaw did not accept it: continue on the agent's page, which
        // offers to retry the materialisation.
        let savedId = agentIdFrom(err.details);
        if (!savedId) savedId = (await findSavedAgent(input.name).catch(() => undefined))?.id;
        if (savedId) {
          navigate(`/agents/${savedId}`, { replace: true, state: { materializeFailed: err.message } satisfies NavState });
        } else {
          setFormError(`${err.message}. Open the agent from the agents list to retry.`);
        }
      } else {
        setFormError(errorText(err));
      }
    },
  });

  return (
    <div className="max-w-4xl">
      <PageHeader title="New agent" />
      <AgentForm
        tools={tools}
        skills={skills}
        saving={save.isPending}
        serverErrors={serverErrors}
        formError={formError}
        submitLabel="Create agent"
        onSubmit={(input) => {
          setServerErrors({});
          setFormError(null);
          save.mutate(input);
        }}
      />
    </div>
  );
}

function EditAgent({ id }: { id: string }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const location = useLocation();
  const nav = location.state as NavState;
  const agentQ = useQuery({ queryKey: ["agent", id], queryFn: () => getAgent(id) });
  const tools = useToolList();
  const skills = useSkills();

  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(nav?.notice ?? null);
  const [unmaterialized, setUnmaterialized] = useState<string | null>(nav?.materializeFailed ?? null);
  const [running, setRunning] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["agents"] });
    void qc.invalidateQueries({ queryKey: ["agent", id] });
    void qc.invalidateQueries({ queryKey: ["agent-runtime", id] });
  };

  const save = useMutation({
    mutationFn: (input: AgentInput) => updateAgent(id, input),
    onSuccess: () => {
      refresh();
      setNotice("Changes saved.");
      setUnmaterialized(null);
    },
    onError: (err) => {
      setNotice(null);
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
      } else if (err instanceof ApiError && err.status === 502) {
        refresh();
        setUnmaterialized(err.message);
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const retry = useMutation({
    mutationFn: () => materializeAgent(id),
    onSuccess: () => {
      refresh();
      setUnmaterialized(null);
      setNotice("Materialised in OpenClaw.");
    },
    onError: (err) => setUnmaterialized(errorText(err)),
  });

  const remove = useMutation({
    mutationFn: () => deleteAgent(id),
    onSuccess: () => {
      qc.removeQueries({ queryKey: ["agent", id] });
      qc.removeQueries({ queryKey: ["agent-runtime", id] });
      void qc.invalidateQueries({ queryKey: ["agents"] });
      navigate("/agents", { replace: true });
    },
  });

  if (agentQ.isLoading) return <Loading label="Loading agent…" />;
  if (agentQ.error || !agentQ.data) {
    return (
      <div className="space-y-3">
        <QueryError what="the agent" error={agentQ.error} onRetry={() => void agentQ.refetch()} />
        <Link to="/agents" className="text-sm underline">
          Back to agents
        </Link>
      </div>
    );
  }
  const agent = agentQ.data;

  return (
    <div className="max-w-4xl space-y-4">
      <PageHeader title={agent.name}>
        <Button onClick={() => setRunning(true)}>Run</Button>
        <Button variant="ghost" onClick={() => setConfirmDelete(true)}>
          Delete
        </Button>
      </PageHeader>
      <p className="-mt-2 text-xs text-zinc-500">
        Created {formatDateTime(agent.created_at)} · Updated {formatDateTime(agent.updated_at)}
      </p>

      {unmaterialized ? (
        <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
          <div>
            <p className="font-medium">The agent is saved but not materialised in OpenClaw.</p>
            <p className="mt-0.5 break-words">{unmaterialized}</p>
          </div>
          <Button size="sm" onClick={() => retry.mutate()} disabled={retry.isPending}>
            {retry.isPending ? "Retrying…" : "Retry materialisation"}
          </Button>
        </div>
      ) : null}
      {notice ? (
        <p role="status" className="rounded-lg border border-emerald-200 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300">
          {notice}
        </p>
      ) : null}

      <RuntimeCard agentId={id} />

      <AgentForm
        agent={agent}
        tools={tools}
        skills={skills}
        memoryFacts={<MemoryFacts agentId={id} enabled={agent.memory_policy?.enabled ?? false} />}
        telegram={<TelegramLine agentId={id} />}
        saving={save.isPending}
        serverErrors={serverErrors}
        formError={formError}
        submitLabel="Save changes"
        onSubmit={(input) => {
          setServerErrors({});
          setFormError(null);
          setNotice(null);
          save.mutate(input);
        }}
      />

      <AgentFiles agentId={id} />

      {running ? <QuickRunDialog agent={agent} onClose={() => setRunning(false)} /> : null}
      {confirmDelete ? (
        <ConfirmDialog
          title={`Delete ${agent.name}?`}
          message="The agent is removed. Runs that already used it keep their history."
          confirmLabel="Delete agent"
          busy={remove.isPending}
          error={remove.error ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate()}
          onCancel={() => {
            remove.reset();
            setConfirmDelete(false);
          }}
        />
      ) : null}
    </div>
  );
}
