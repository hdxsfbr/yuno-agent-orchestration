import { useEffect, useRef, useState } from "react";
import { subscribe, type SSEEvent } from "./api";

export type Connection = "connecting" | "open" | "reconnecting";

/**
 * Keeps an SSE subscription open for as long as the component is mounted and reports the
 * connection state. The handler may change between renders without resubscribing.
 */
export function useEventStream(path: string, onEvent: (e: SSEEvent) => void): Connection {
  const [connection, setConnection] = useState<Connection>("connecting");
  const handler = useRef(onEvent);
  useEffect(() => {
    handler.current = onEvent;
  });

  useEffect(() => {
    let active = true;
    let everOpened = false;
    setConnection("connecting");
    const stop = subscribe(
      path,
      (e) => handler.current(e),
      (s) => {
        if (!active) return;
        if (s === "open") {
          everOpened = true;
          setConnection("open");
        } else {
          setConnection(everOpened ? "reconnecting" : "connecting");
        }
      },
    );
    return () => {
      active = false;
      stop();
    };
  }, [path]);

  return connection;
}
