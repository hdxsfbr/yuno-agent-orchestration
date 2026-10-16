import { useEffect, useRef } from "react";
import { useQueryClient, type QueryKey } from "@tanstack/react-query";
import { useEventStream, type Connection } from "./useEventStream";

/** How long to gather events before refreshing a list. */
export const REFRESH_DELAY_MS = 500;

/**
 * Keeps a list current from the global event stream: an event (any, or only those named in
 * `events`) refreshes the query after a short delay, so a burst costs at most two
 * requests a second however busy the stream is.
 */
export function useRefreshOnEvents(queryKey: QueryKey, events?: readonly string[]): Connection {
  const qc = useQueryClient();
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      clearTimeout(timer.current);
      timer.current = undefined;
    },
    [],
  );
  return useEventStream("/api/events", (e) => {
    if (events && !events.includes(e.event)) return;
    if (timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = undefined;
      void qc.invalidateQueries({ queryKey });
    }, REFRESH_DELAY_MS);
  });
}
