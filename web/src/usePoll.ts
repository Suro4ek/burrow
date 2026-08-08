import { useCallback, useEffect, useRef, useState } from "react";
import { isUnauthorized } from "./api";

export interface Poll<T> {
  data: T | undefined;
  error: string | undefined;
  loading: boolean;
  refresh: () => void;
}

/**
 * Polls an endpoint on an interval and keeps the last good value visible while
 * refreshing, so the tables never flash empty.
 *
 * Polling pauses while the tab is hidden: an admin panel left open in a
 * background tab should not keep hitting the server every couple of seconds.
 */
export function usePoll<T>(
  fetcher: () => Promise<T>,
  intervalMs: number,
  onUnauthorized: () => void,
): Poll<T> {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(true);

  // Keep the latest callbacks in refs so changing them never restarts the
  // interval, which would reset the polling cadence on every render.
  const fetcherRef = useRef(fetcher);
  const unauthRef = useRef(onUnauthorized);
  fetcherRef.current = fetcher;
  unauthRef.current = onUnauthorized;

  const alive = useRef(true);

  const run = useCallback(async () => {
    try {
      const next = await fetcherRef.current();
      if (!alive.current) return;
      setData(next);
      setError(undefined);
    } catch (err) {
      if (!alive.current) return;
      if (isUnauthorized(err)) {
        unauthRef.current();
        return;
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (alive.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    alive.current = true;
    void run();

    const tick = () => {
      if (!document.hidden) void run();
    };
    const id = window.setInterval(tick, intervalMs);
    // Refresh immediately on return so the view is never stale on focus.
    document.addEventListener("visibilitychange", tick);

    return () => {
      alive.current = false;
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [run, intervalMs]);

  return { data, error, loading, refresh: run };
}
