import { useCallback } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listSchedules, type Schedule, type SSEEvent } from "./api";
import { useEventStream, type Connection } from "./useEventStream";

export const schedulesKey = ["schedules"] as const;

export const useSchedules = () => useQuery({ queryKey: schedulesKey, queryFn: listSchedules });

/** Replaces the schedule with the same id in the cached list. */
export function replaceSchedule(list: Schedule[] | undefined, next: Schedule): Schedule[] | undefined {
  return list?.map((s) => (s.id === next.id ? { ...s, ...next } : s));
}

/** A fired schedule updates its row at once (next and last run) and adds the run to the Runs list. */
export function useScheduleEvents(): Connection {
  const qc = useQueryClient();
  const onEvent = useCallback(
    (e: SSEEvent) => {
      if (e.event !== "schedule.fired") return;
      const schedule = (e.data as { schedule?: Schedule } | null)?.schedule;
      if (schedule?.id) {
        qc.setQueryData<Schedule[]>(schedulesKey, (old) => replaceSchedule(old, schedule));
      }
      void qc.invalidateQueries({ queryKey: schedulesKey });
      void qc.invalidateQueries({ queryKey: ["runs"] });
    },
    [qc],
  );
  return useEventStream("/api/events", onEvent);
}
