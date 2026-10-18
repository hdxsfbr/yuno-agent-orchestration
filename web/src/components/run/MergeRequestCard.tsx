import { useEffect, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { mergeMergeRequest, syncMergeRequest, type MergeRequest } from "@/lib/api";
import { formatAgo, formatDateTime } from "@/lib/format";
import { isStale, mergeBlockedReason, requestNoun, requestRef, threadsText } from "@/lib/mergeRequest";
import { providerLabel } from "@/lib/project";
import { useNow } from "@/lib/useNow";
import Button from "../Button";
import { Card } from "../Card";
import { CIStatusBadge, MergeRequestStateBadge } from "../MergeRequestBadges";
import { ConfirmDialog } from "../Modal";
import { InlineError, errorText } from "../QueryState";

/** A record last refreshed longer ago than this is refreshed again when the card appears. */
const FRESH_MS = 30_000;

type Props = {
  mr: MergeRequest;
  /** Called with the record the API returns after a sync or a merge. */
  onChange: (mr: MergeRequest) => void;
};

/**
 * The merge or pull request the run opened: where it stands (state, pipeline, review threads),
 * a way to refresh it and, while it is open with a pipeline that passed or does not exist,
 * a way to merge it.
 */
export default function MergeRequestCard({ mr, onChange }: Props) {
  const now = useNow(true, 10_000);
  const [confirming, setConfirming] = useState(false);
  const sync = useMutation({ mutationFn: () => syncMergeRequest(mr.id), onSuccess: (record) => onChange(record) });
  const merge = useMutation({
    mutationFn: () => mergeMergeRequest(mr.id),
    onSuccess: (record) => {
      setConfirming(false);
      onChange(record);
    },
    onError: () => {
      // The message stays in the card; the request may have changed on the provider meanwhile, so the record is read again.
      setConfirming(false);
      syncMergeRequest(mr.id).then(onChange, () => undefined);
    },
  });

  // An open record is refreshed when the card appears, unless that happened a moment ago.
  useEffect(() => {
    if (mr.state === "open" && isStale(mr, Date.now(), FRESH_MS)) sync.mutate();
    // Once per record: later renders show what the sync returned.
  }, [mr.id]);

  const noun = requestNoun(mr.provider);
  const ref = requestRef(mr);
  const provider = providerLabel(mr.provider);
  const busy = sync.isPending || merge.isPending;
  const blocked = mergeBlockedReason(mr);
  const final = mr.state !== "open";
  const finalAt = mr.closed_at ?? mr.updated_at;

  return (
    <section aria-label={noun}>
      <Card>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <a href={mr.url} target="_blank" rel="noreferrer" className="text-sm font-semibold underline">
                {noun} {ref}
              </a>
              <MergeRequestStateBadge state={mr.state} />
              {final ? <span className="text-xs text-zinc-500">{formatDateTime(finalAt)}</span> : null}
              <CIStatusBadge status={mr.ci_status} labelled />
            </div>
            <p className="mt-1.5 break-words text-sm">
              <a href={mr.url} target="_blank" rel="noreferrer" className="hover:underline">
                {mr.title}
              </a>
            </p>
            <p className="mt-1 font-mono text-xs text-zinc-500">
              {mr.source_branch} → {mr.target_branch}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <Button onClick={() => sync.mutate()} disabled={busy}>
              {sync.isPending ? "Syncing…" : "Sync"}
            </Button>
            <Button variant="primary" onClick={() => setConfirming(true)} disabled={busy || blocked !== undefined} title={blocked}>
              {merge.isPending ? "Merging…" : "Merge"}
            </Button>
          </div>
        </div>

        <p className="mt-3 text-sm">{threadsText(mr.unresolved_threads)}</p>
        {mr.last_sync_error ? <p className="mt-1 text-xs text-zinc-500">{mr.last_sync_error}</p> : null}
        <p className="mt-1 text-xs text-zinc-500">{mr.synced_at ? `Synced ${formatAgo(mr.synced_at, now)}` : "Not synced yet"}</p>

        {sync.error ? (
          <div className="mt-3">
            <InlineError>Could not sync the {noun.toLowerCase()}: {errorText(sync.error)}</InlineError>
          </div>
        ) : null}
        {merge.error ? (
          <div className="mt-3">
            <InlineError>{errorText(merge.error)}</InlineError>
          </div>
        ) : null}
      </Card>

      {confirming ? (
        <ConfirmDialog
          title={`Merge ${ref} into ${mr.target_branch}?`}
          message={
            <>
              <p className="break-words font-medium text-zinc-900 dark:text-zinc-100">{mr.title}</p>
              <p className="mt-2">
                {mr.provider === "github"
                  ? `The pull request is merged on ${provider}.`
                  : `The ${noun.toLowerCase()} is merged on ${provider} and its source branch deleted.`}
              </p>
            </>
          }
          confirmLabel="Merge"
          confirmVariant="primary"
          busy={merge.isPending}
          onConfirm={() => merge.mutate()}
          onCancel={() => setConfirming(false)}
        />
      ) : null}
    </section>
  );
}
