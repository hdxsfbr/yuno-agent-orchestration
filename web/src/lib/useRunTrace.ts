import { useCallback, useEffect, useState } from "react";
import { getTrace, isTerminalStatus, subscribe, type Approval, type MergeRequest, type Run, type SSEEvent } from "./api";
import type { Connection } from "./useEventStream";
import { applyEvent, applyEvents, toRunView, type RunView } from "./trace";

/** How long to wait for the stream to open before loading the trace anyway. */
const STREAM_WAIT_MS = 2000;
/** Events held back while the trace is loading; the oldest are dropped past this. */
const MAX_BUFFERED_EVENTS = 2000;
/** The server replays events after the given id; id 1 asks for the run's whole history. */
const REPLAY_FROM_START = "1";

export type RunTraceState = {
  view: RunView | null;
  error: Error | null;
  connection: Connection;
  /** Starts over: reconnects the stream and reloads the trace. */
  retry: () => void;
  /** Applies a run returned by the API (for example after a cancel request). */
  applyRun: (run: Run) => void;
  /** Applies an approval returned by the API after a decision, ahead of the stream's event. */
  applyApproval: (approval: Approval) => void;
  /** Puts a merge request record the API returned (a sync, a merge, a lookup) on the run. */
  applyMergeRequest: (mr: MergeRequest) => void;
};

/**
 * The trace of one run kept live by that run's event stream.
 *
 * The stream is opened first and the trace loaded once it is open, so nothing that
 * happens between the two is lost; events that arrive while the trace loads are buffered
 * and applied on top of it. The trace is loaded again after every reconnect and when the
 * run reaches a final status, which re-baselines anything the stream missed. Log lines
 * exist only on the stream, so the stream is opened with a replay from the start; so do the
 * marks of a retry (`run.retry`).
 */
export function useRunTrace(runId: string): RunTraceState {
  const [view, setView] = useState<RunView | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [connection, setConnection] = useState<Connection>("connecting");
  const [attempt, setAttempt] = useState(0);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);

  useEffect(() => {
    let cancelled = false;
    let started = false;
    let loading = false;
    let loaded = false;
    let reloadAfter = false;
    let everOpened = false;
    let buffer: SSEEvent[] = [];
    let terminalTimer: ReturnType<typeof setTimeout> | undefined;

    setView(null);
    setError(null);
    setConnection("connecting");

    const load = async () => {
      if (loading) {
        reloadAfter = true;
        return;
      }
      loading = true;
      try {
        const trace = await getTrace(runId);
        if (cancelled) return;
        const queued = buffer;
        buffer = [];
        setError(null);
        setView((prev) => {
          // A record never goes away, so one the view already holds survives a trace that does not carry it.
          const loaded = !trace.run.merge_request && prev?.run.merge_request ? { ...trace, run: { ...trace.run, merge_request: prev.run.merge_request } } : trace;
          return applyEvents(toRunView(loaded, prev?.logs ?? [], prev?.retries ?? []), queued);
        });
        loaded = true;
      } catch (e) {
        if (!cancelled && !loaded) setError(e instanceof Error ? e : new Error(String(e)));
      } finally {
        loading = false;
        if (reloadAfter && !cancelled) {
          reloadAfter = false;
          void load();
        }
      }
    };

    const start = () => {
      started = true;
      void load();
    };

    const onEvent = (e: SSEEvent) => {
      const live = loaded && !loading;
      if (live) {
        setView((v) => (v ? applyEvent(v, e) : v));
      } else {
        buffer.push(e);
        if (buffer.length > MAX_BUFFERED_EVENTS) buffer = buffer.slice(buffer.length - MAX_BUFFERED_EVENTS);
      }
      // A final status re-reads the trace, also when the event was buffered behind a load
      // that may have read rows before the run finished (load() queues behind it).
      if (e.event === "run.status") {
        const run = (e.data as { run?: Run } | null)?.run;
        if (run && run.id === runId && isTerminalStatus(run.status)) {
          clearTimeout(terminalTimer);
          terminalTimer = setTimeout(() => void load(), 300);
        }
      }
    };

    const waitTimer = setTimeout(() => {
      if (!started) start();
    }, STREAM_WAIT_MS);

    const stop = subscribe(
      `/api/runs/${encodeURIComponent(runId)}/events`,
      onEvent,
      (s) => {
        if (cancelled) return;
        if (s === "open") {
          everOpened = true;
          setConnection("open");
          if (!started) start();
          else void load();
        } else {
          setConnection(everOpened ? "reconnecting" : "connecting");
        }
      },
      { lastEventId: REPLAY_FROM_START },
    );

    return () => {
      cancelled = true;
      clearTimeout(waitTimer);
      clearTimeout(terminalTimer);
      stop();
    };
  }, [runId, attempt]);

  const applyRun = useCallback((run: Run) => {
    setView((v) => (v && v.run.id === run.id ? applyEvent(v, { event: "run.status", data: { run } }) : v));
  }, []);

  const applyApproval = useCallback((approval: Approval) => {
    setView((v) => (v && v.run.id === approval.run_id ? applyEvent(v, { event: "approval.decided", data: { approval } }) : v));
  }, []);

  const applyMergeRequest = useCallback((mr: MergeRequest) => {
    setView((v) => (v && v.run.id === mr.run_id ? { ...v, run: { ...v.run, merge_request: mr } } : v));
  }, []);

  return { view, error, connection, retry, applyRun, applyApproval, applyMergeRequest };
}
