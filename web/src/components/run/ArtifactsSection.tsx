import { Fragment, useEffect, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { downloadArtifact, isTerminalStatus, type Artifact, type RunStatus } from "@/lib/api";
import { baseName, previewKind, saveBlob, type PreviewKind } from "@/lib/artifact";
import { formatAgo, formatBytes, formatDateTime } from "@/lib/format";
import { useNow } from "@/lib/useNow";
import { useRunArtifacts } from "@/lib/useRunArtifacts";
import Button from "../Button";
import { EmptyState, InlineError, Loading, QueryError, errorText } from "../QueryState";

type Props = {
  runId: string;
  /** The run's status: a run that is over delivers nothing more, which the empty state says. */
  status: RunStatus;
};

/** The files the run delivered: what each is, where it came from, and a way to read it or keep it. */
export default function ArtifactsSection({ runId, status }: Props) {
  const files = useRunArtifacts(runId);
  const now = useNow(true, 30_000);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const download = useMutation({
    mutationFn: async (file: Artifact) => saveBlob(await downloadArtifact(runId, file.id), baseName(file.path)),
  });

  const list = files.data ?? [];
  const toggle = (id: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });

  return (
    <section id="artifacts" aria-labelledby="artifacts-heading">
      <h2 id="artifacts-heading" className="mb-2 text-base font-semibold">
        Artifacts
        {list.length > 0 ? <span className="ml-2 text-sm font-normal text-zinc-500">{list.length}</span> : null}
      </h2>

      {files.isLoading ? <Loading label="Loading files…" /> : null}
      {files.error && !files.data ? <QueryError what="the delivered files" error={files.error} onRetry={() => void files.refetch()} /> : null}
      {files.data && list.length === 0 ? <EmptyState>{isTerminalStatus(status) ? "No files were delivered." : "No files delivered yet."}</EmptyState> : null}

      {list.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Files delivered by this run</caption>
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th scope="col" className="px-4 py-2 font-medium">
                  File
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  Size
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  Step
                </th>
                <th scope="col" className="px-4 py-2 font-medium">
                  Updated
                </th>
                <th scope="col" className="px-4 py-2">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {list.map((file) => {
                const kind = previewKind(file);
                const showing = kind !== undefined && open.has(file.id);
                const downloading = download.isPending && download.variables?.id === file.id;
                return (
                  <Fragment key={file.id}>
                    <tr>
                      <th scope="row" className="break-all px-4 py-2 font-mono text-xs font-medium">
                        {file.path}
                      </th>
                      <td className="whitespace-nowrap px-4 py-2 tabular-nums">{formatBytes(file.size)}</td>
                      <td className="whitespace-nowrap px-4 py-2">
                        <span className="font-mono text-xs">{file.node_id}</span> visit {file.visit}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2 text-zinc-500" title={formatDateTime(file.updated_at)}>
                        {formatAgo(file.updated_at, now)}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2 text-right">
                        <div className="flex justify-end gap-2">
                          {kind ? (
                            <Button size="sm" aria-expanded={showing} aria-label={`${showing ? "Hide preview of" : "Preview"} ${file.path}`} onClick={() => toggle(file.id)}>
                              {showing ? "Hide" : "Preview"}
                            </Button>
                          ) : null}
                          <Button size="sm" aria-label={`Download ${file.path}`} onClick={() => download.mutate(file)} disabled={download.isPending}>
                            {downloading ? "Downloading…" : "Download"}
                          </Button>
                        </div>
                      </td>
                    </tr>
                    {showing ? (
                      <tr>
                        <td colSpan={5} className="bg-zinc-50 px-4 py-3 dark:bg-zinc-950/40">
                          <Preview runId={runId} file={file} kind={kind} />
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      {download.error ? (
        <div className="mt-3">
          <InlineError>
            Could not download {download.variables ? baseName(download.variables.path) : "the file"}: {errorText(download.error)}
          </InlineError>
        </div>
      ) : null}
    </section>
  );
}

type Loaded = { state: "loading" } | { state: "failed"; error: unknown } | { state: "text"; text: string } | { state: "image"; url: string };

/** The file shown in place: its text in a `<pre>` (never as markup), or an image from a blob URL. It is read again when the file changes. */
function Preview({ runId, file, kind }: { runId: string; file: Artifact; kind: PreviewKind }) {
  const [loaded, setLoaded] = useState<Loaded>({ state: "loading" });

  useEffect(() => {
    let current = true;
    let url: string | undefined;
    setLoaded((prev) => (prev.state === "loading" ? prev : { state: "loading" }));
    downloadArtifact(runId, file.id)
      .then(async (blob) => {
        if (kind === "text") return { state: "text", text: await blob.text() } as const;
        url = URL.createObjectURL(blob);
        return { state: "image", url } as const;
      })
      .then(
        (result) => {
          if (current) setLoaded(result);
          else if (url) URL.revokeObjectURL(url);
        },
        (error: unknown) => {
          if (current) setLoaded({ state: "failed", error });
        },
      );
    return () => {
      current = false;
      if (url) URL.revokeObjectURL(url);
    };
  }, [runId, file.id, file.sha256, kind]);

  return (
    <div role="region" aria-label={`Preview of ${file.path}`}>
      {loaded.state === "loading" ? <Loading label="Loading preview…" /> : null}
      {loaded.state === "failed" ? <InlineError>Could not load the preview: {errorText(loaded.error)}</InlineError> : null}
      {loaded.state === "text" ? (
        <pre tabIndex={0} className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md border border-zinc-200 bg-white p-3 font-mono text-xs dark:border-zinc-800 dark:bg-zinc-900">
          {loaded.text}
        </pre>
      ) : null}
      {loaded.state === "image" ? <img src={loaded.url} alt={file.path} className="max-h-96 max-w-full rounded-md border border-zinc-200 dark:border-zinc-800" /> : null}
    </div>
  );
}
