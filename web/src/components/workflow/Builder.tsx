import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "react-router";
import {
  ApiError,
  createWorkflow,
  instantiateTemplate,
  listAgents,
  updateWorkflow,
  validateWorkflow,
  type NodeKind,
  type Position,
  type Workflow,
  type WorkflowDefinition,
  type WorkflowInput,
} from "@/lib/api";
import {
  addEdge,
  addNode,
  isShippedTemplate,
  moveEdge,
  newDefinition,
  removeEdge,
  removeNode,
  renameNode,
  setPosition,
  setPositions,
  updateEdge,
  updateNode,
} from "@/lib/workflow";
import { autoLayout, nextPosition, withPositions } from "@/lib/workflowLayout";
import { clearFor, groupErrors, noErrors, type Touch, type WorkflowErrors } from "@/lib/workflowErrors";
import { errorsFor } from "@/lib/agentForm";
import { formatDateTime } from "@/lib/format";
import { Badge } from "../Badge";
import Button from "../Button";
import { Card } from "../Card";
import { CheckboxField, TextField } from "../fields";
import { ConfirmDialog } from "../Modal";
import { InlineError, errorText } from "../QueryState";
import Canvas from "./Canvas";
import EdgePanel from "./EdgePanel";
import JsonEditor from "./JsonEditor";
import LimitsPanel from "./LimitsPanel";
import NodePanel from "./NodePanel";
import Outline from "./Outline";
import Problems from "./Problems";
import RunWorkflowDialog from "./RunWorkflowDialog";
import type { Selection } from "./selection";

type Draft = { name: string; description: string; definition: WorkflowDefinition };

const toInput = (d: Draft): WorkflowInput => ({ name: d.name.trim(), description: d.description, definition: d.definition });

/** The draft a workflow opens as: remembered positions kept, automatic layout for the rest. */
function prepare(w?: Workflow): Draft {
  const definition = w ? w.definition : newDefinition();
  return { name: w?.name ?? "", description: w?.description ?? "", definition: setPositions(definition, withPositions(definition)) };
}

/** The definition without its project flag (the key is left out, not set to false). */
function omitProject(d: WorkflowDefinition): WorkflowDefinition {
  const rest = { ...d };
  delete rest.project;
  return rest;
}

function Section({ title, children, aside }: { title: string; children: ReactNode; aside?: ReactNode }) {
  return (
    <Card>
      <div className="mb-3 flex items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{title}</h2>
        {aside}
      </div>
      {children}
    </Card>
  );
}

type NavState = { notice?: string } | null;

/** The workflow editor. `workflow` is undefined for a new one. */
export default function Builder({ workflow }: { workflow?: Workflow }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const location = useLocation();
  const agentsQ = useQuery({ queryKey: ["agents"], queryFn: listAgents });
  const agents = useMemo(() => agentsQ.data ?? [], [agentsQ.data]);
  const agentNames = useMemo(() => Object.fromEntries(agents.map((a) => [a.id, a.name])), [agents]);

  const [initial] = useState(() => prepare(workflow));
  const [draft, setDraft] = useState<Draft>(initial);
  const [savedSnapshot, setSavedSnapshot] = useState(() => JSON.stringify(toInput(initial)));
  const [selected, setSelected] = useState<Selection | null>(null);
  // Bumped when the user opens something else, so the side panel starts afresh; a reorder
  // or a rename moves the same thing to a new place and keeps the panel (and focus) as is.
  const [panelKey, setPanelKey] = useState(0);
  const [errors, setErrors] = useState<WorkflowErrors>(noErrors);
  const [formError, setFormError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>((location.state as NavState)?.notice ?? null);
  const [nameError, setNameError] = useState<string | null>(null);
  const [layoutVersion, setLayoutVersion] = useState(0);
  const [blockers, setBlockers] = useState<Record<string, string>>({});
  const [running, setRunning] = useState(false);
  const [deletingNode, setDeletingNode] = useState<string | null>(null);

  const def = draft.definition;
  const isNew = !workflow;
  const dirty = JSON.stringify(toInput(draft)) !== savedSnapshot;
  const blocked = Object.values(blockers);
  const shipped = workflow ? isShippedTemplate({ template_key: workflow.template_key, definition: def }) : false;

  const onBlocker = useCallback((key: string, message: string | null) => {
    setBlockers((b) => {
      if ((b[key] ?? null) === message) return b;
      const next = { ...b };
      if (message) next[key] = message;
      else delete next[key];
      return next;
    });
  }, []);

  // A page with unsaved edits asks before it is closed.
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  /** An edit: server messages about what it touched go away, or all of them when indexes may shift. */
  const edit = useCallback((fn: (d: WorkflowDefinition) => WorkflowDefinition, touched?: Touch) => {
    setDraft((d) => ({ ...d, definition: fn(d.definition) }));
    setErrors((e) => (touched ? clearFor(e, touched) : noErrors()));
    setNotice(null);
    setFormError(null);
  }, []);

  const onServerError = (err: unknown) => {
    setNotice(null);
    if (err instanceof ApiError && err.status === 422) {
      const grouped = groupErrors(err.details);
      setErrors(grouped);
      setFormError(grouped.list.length === 0 ? err.message : null);
    } else {
      setFormError(errorText(err));
    }
  };

  const save = useMutation({
    mutationFn: (input: WorkflowInput) => (workflow ? updateWorkflow(workflow.id, input) : createWorkflow(input)),
    onSuccess: (saved, input) => {
      setSavedSnapshot(JSON.stringify(input));
      setErrors(noErrors());
      setFormError(null);
      void qc.invalidateQueries({ queryKey: ["workflows"] });
      qc.setQueryData(["workflow", saved.id], saved);
      if (workflow) setNotice("Changes saved.");
      else navigate(`/workflows/${saved.id}`, { replace: true, state: { notice: "Workflow created." } satisfies NavState });
    },
    onError: onServerError,
  });

  const validate = useMutation({
    mutationFn: () => validateWorkflow(def),
    onSuccess: () => {
      setErrors(noErrors());
      setFormError(null);
      setNotice("The definition is valid.");
    },
    onError: onServerError,
  });

  const copy = useMutation({
    mutationFn: () => instantiateTemplate(workflow?.template_key ?? ""),
    onSuccess: (wf) => {
      void qc.invalidateQueries({ queryKey: ["workflows"] });
      navigate(`/workflows/${wf.id}`);
    },
    onError: onServerError,
  });

  const trySave = () => {
    if (draft.name.trim() === "") {
      setNameError("Name is required");
      return;
    }
    setNameError(null);
    setNotice(null);
    save.mutate(toInput(draft));
  };

  // --- structure edits -------------------------------------------------------------------

  const select = useCallback((s: Selection | null) => {
    setSelected(s);
    setPanelKey((k) => k + 1);
  }, []);

  const addNodeOfKind = (kind: NodeKind) => {
    const { def: next, id } = addNode(def, kind, nextPosition(def, selected?.type === "node" ? selected.id : undefined));
    edit(() => next);
    select({ type: "node", id });
  };

  const connect = (from: string, to: string) => {
    const r = addEdge(def, from, to);
    edit(() => r.def);
    select({ type: "edge", index: r.index });
  };

  const reorder = (index: number, delta: -1 | 1) => {
    const r = moveEdge(def, index, delta);
    if (r.def === def) return;
    edit(() => r.def);
    if (selected?.type === "edge") {
      if (selected.index === index) setSelected({ type: "edge", index: r.index });
      else if (selected.index === r.index) setSelected({ type: "edge", index });
    }
  };

  const dropNode = (id: string) => {
    edit((d) => removeNode(d, id));
    select(null);
    setDeletingNode(null);
  };

  const dropEdge = (index: number) => {
    edit((d) => removeEdge(d, index));
    select(null);
  };

  const moveNode = useCallback((id: string, position: Position) => {
    setDraft((d) => ({ ...d, definition: setPosition(d.definition, id, position) }));
  }, []);

  const resetLayout = () => {
    setDraft((d) => ({ ...d, definition: setPositions(d.definition, autoLayout(d.definition)) }));
    setLayoutVersion((v) => v + 1);
  };

  // --- what the side panel shows ---------------------------------------------------------

  const nodeIndex = selected?.type === "node" ? def.nodes.findIndex((n) => n.id === selected.id) : -1;
  const edgeIndex = selected?.type === "edge" && def.edges[selected.index] ? selected.index : -1;
  const node = nodeIndex >= 0 ? def.nodes[nodeIndex] : undefined;
  const edge = edgeIndex >= 0 ? def.edges[edgeIndex] : undefined;
  const busy = save.isPending || validate.isPending;

  let panel: ReactNode = <p className="text-sm text-zinc-500">Select a node or an edge, on the canvas or in the outline, to edit it.</p>;
  if (node) {
    panel = (
      <NodePanel
        key={panelKey}
        def={def}
        node={node}
        agents={agents}
        errors={errors.nodes[nodeIndex] ?? {}}
        edgeErrors={errors.edges}
        onChange={(patch) => edit((d) => updateNode(d, node.id, patch), { type: "node", index: nodeIndex })}
        onRename={(to) => {
          edit((d) => renameNode(d, node.id, to), { type: "node", index: nodeIndex });
          setSelected({ type: "node", id: to });
        }}
        onDelete={() => setDeletingNode(node.id)}
        onAddEdge={(to) => connect(node.id, to)}
        onSelectEdge={(index) => select({ type: "edge", index })}
        onMoveEdge={reorder}
        onBlocker={onBlocker}
      />
    );
  } else if (edge) {
    panel = (
      <EdgePanel
        key={panelKey}
        def={def}
        edge={edge}
        index={edgeIndex}
        errors={errors.edges[edgeIndex] ?? {}}
        onChange={(patch) => edit((d) => updateEdge(d, edgeIndex, patch), { type: "edge", index: edgeIndex })}
        onMove={(delta) => reorder(edgeIndex, delta)}
        onDelete={() => dropEdge(edgeIndex)}
        onSelectNode={(id) => select({ type: "node", id })}
        onBlocker={onBlocker}
      />
    );
  }

  const canRun = !isNew && !dirty && !shipped && blocked.length === 0;
  const hasStart = def.nodes.some((n) => n.kind === "start");

  return (
    <div className="space-y-4">
      <Link to="/workflows" className="text-sm text-zinc-500 hover:underline">
        ← Workflows
      </Link>

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1 space-y-1">
          <h1 className="text-xl font-semibold">{isNew ? "New workflow" : (workflow?.name ?? "Workflow")}</h1>
          {workflow ? (
            <p className="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>Version {workflow.version}</span>
              <span>Updated {formatDateTime(workflow.updated_at)}</span>
              {dirty ? <Badge tone="amber">Unsaved changes</Badge> : null}
            </p>
          ) : dirty ? (
            <Badge tone="amber">Not saved yet</Badge>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button onClick={() => validate.mutate()} disabled={busy || blocked.length > 0}>
            {validate.isPending ? "Validating…" : "Validate"}
          </Button>
          <Button variant="primary" onClick={trySave} disabled={busy || blocked.length > 0 || shipped || (!isNew && !dirty)}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
          <Button onClick={() => setRunning(true)} disabled={!canRun} title={dirty ? "Save your changes to run this workflow" : undefined}>
            Run
          </Button>
        </div>
      </div>

      {shipped ? (
        <div role="note" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-blue-200 bg-blue-50 p-3 text-sm text-blue-900 dark:border-blue-900 dark:bg-blue-950/40 dark:text-blue-200">
          <p>This is a shipped template. It names its agents instead of choosing them, so it cannot run or be saved as it is.</p>
          <Button size="sm" onClick={() => copy.mutate()} disabled={copy.isPending}>
            {copy.isPending ? "Creating…" : "Create editable copy"}
          </Button>
        </div>
      ) : null}
      {notice ? (
        <p role="status" className="rounded-lg border border-emerald-200 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300">
          {notice}
        </p>
      ) : null}
      {formError ? <InlineError>{formError}</InlineError> : null}
      {blocked.length > 0 ? (
        <p role="alert" className="rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
          Fix this before saving or running: {blocked.join("; ")}.
        </p>
      ) : null}
      <Problems problems={errors.list} def={def} onSelect={select} />

      <Card className="grid gap-4 sm:grid-cols-2">
        <TextField
          label="Name"
          required
          value={draft.name}
          maxLength={80}
          errors={[...(nameError ? [nameError] : []), ...errorsFor(errors.other, "name")]}
          onChange={(name) => {
            setNameError(null);
            setDraft((d) => ({ ...d, name }));
          }}
        />
        <TextField
          label="Description"
          value={draft.description}
          errors={errorsFor(errors.other, "description")}
          onChange={(description) => setDraft((d) => ({ ...d, description }))}
        />
      </Card>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_26rem]">
        <div className="min-w-0 space-y-4">
          <div className="space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" onClick={() => addNodeOfKind("agent")}>
                Add agent
              </Button>
              <Button size="sm" onClick={() => addNodeOfKind("approval")}>
                Add approval
              </Button>
              <Button size="sm" onClick={() => addNodeOfKind("end")}>
                Add end
              </Button>
              {hasStart ? null : (
                <Button size="sm" onClick={() => addNodeOfKind("start")}>
                  Add start
                </Button>
              )}
              <Button size="sm" variant="ghost" onClick={resetLayout}>
                Auto layout
              </Button>
              <span className="ml-auto text-xs text-zinc-500">Drag from one node's right handle to another's left handle to connect them.</span>
            </div>
            <Canvas
              def={def}
              agentNames={agentNames}
              selected={selected}
              errors={errors}
              layoutVersion={layoutVersion}
              onSelect={select}
              onMove={moveNode}
              onConnect={connect}
            />
          </div>
          <Section title="Outline">
            <Outline def={def} selected={selected} errors={errors} onSelect={select} />
          </Section>
        </div>
        <aside className="min-w-0">
          <Card className="xl:sticky xl:top-4">{panel}</Card>
        </aside>
      </div>

      <Section title="Project repository">
        <CheckboxField
          checked={!!def.project}
          onChange={(v) => edit((d) => (v ? { ...d, project: true } : omitProject(d)), { type: "other", prefix: "project" })}
          label="Runs work on the project repository"
          hint="Each run of this workflow names a project, chosen when the run starts. The project's repository is cloned into the first agent's run directory, later agents receive the checkout and the Deployer can open a merge request."
        />
      </Section>

      <Section title="Limits">
        <p className="mb-3 text-sm text-zinc-500">A run stops, and is marked accordingly, as soon as it reaches one of these. Empty fields use the platform default.</p>
        <LimitsPanel
          limits={def.limits}
          errors={errors.other}
          onBlocker={onBlocker}
          onChange={(limits) => edit((d) => ({ ...d, limits }), { type: "other", prefix: "limits" })}
        />
      </Section>

      <Section title="Schemas">
        <div className="grid gap-4 lg:grid-cols-2">
          <JsonEditor
            label="Input schema"
            hint="What a run starts with. The Run form is built from its properties."
            rows={12}
            value={def.input_schema}
            blockerKey="input_schema"
            onBlocker={onBlocker}
            errors={errorsFor(errors.other, "input_schema")}
            onChange={(v) => edit((d) => ({ ...d, input_schema: v }), { type: "other", prefix: "input_schema" })}
          />
          <JsonEditor
            label="State schema"
            hint="The shape of the state the nodes' results are collected into."
            rows={12}
            value={def.state_schema}
            blockerKey="state_schema"
            onBlocker={onBlocker}
            errors={errorsFor(errors.other, "state_schema")}
            onChange={(v) => edit((d) => ({ ...d, state_schema: v }), { type: "other", prefix: "state_schema" })}
          />
        </div>
      </Section>

      {running && workflow ? <RunWorkflowDialog workflow={{ id: workflow.id, name: workflow.name, definition: def }} onClose={() => setRunning(false)} /> : null}
      {deletingNode ? (
        <ConfirmDialog
          title={`Delete node ${deletingNode}?`}
          message="The node and every edge that touches it are removed."
          confirmLabel="Delete node"
          onConfirm={() => dropNode(deletingNode)}
          onCancel={() => setDeletingNode(null)}
        />
      ) : null}
    </div>
  );
}
