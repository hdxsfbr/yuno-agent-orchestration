import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { getWorkflow } from "@/lib/api";
import { Loading, QueryError } from "@/components/QueryState";
import Builder from "@/components/workflow/Builder";

export default function WorkflowBuilderPage() {
  const { id } = useParams();
  return id ? <EditWorkflow key={id} id={id} /> : <Builder />;
}

function EditWorkflow({ id }: { id: string }) {
  const q = useQuery({ queryKey: ["workflow", id], queryFn: () => getWorkflow(id) });
  if (q.isLoading) return <Loading label="Loading workflow…" />;
  if (q.error || !q.data) {
    return (
      <div className="space-y-3">
        <QueryError what="the workflow" error={q.error} onRetry={() => void q.refetch()} />
        <Link to="/workflows" className="text-sm underline">
          Back to workflows
        </Link>
      </div>
    );
  }
  return <Builder workflow={q.data} />;
}
