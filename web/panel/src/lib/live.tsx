import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import type { ServerEvent, Snapshot } from './types';

export interface HistoryPoint {
  t: number; // unix seconds
  viewers: number;
  in: number;
  out: number;
}

interface LiveValue {
  snapshot: Snapshot | null;
  connected: boolean;
  events: ServerEvent[]; // newest first
  /** Recent totals sampled from snapshots, for sparklines (last 15 min). */
  history: HistoryPoint[];
  /** Per-stream bitrate-in history, for tile meters. */
  streamHistory: Record<string, number[]>;
}

const LiveContext = createContext<LiveValue | null>(null);
const MAX_EVENTS = 60;
const MAX_HISTORY = 450; // 15 min at 2 s
const MAX_STREAM_HISTORY = 30;

/** Subscribes to /api/v1/events (Server-Sent Events) while logged in. */
export function LiveProvider({ children }: { children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [connected, setConnected] = useState(false);
  const [events, setEvents] = useState<ServerEvent[]>([]);
  const [history, setHistory] = useState<HistoryPoint[]>([]);
  const streamHistory = useRef<Record<string, number[]>>({});

  useEffect(() => {
    let es: EventSource | null = null;
    let retry: number | undefined;
    let closed = false;

    const open = () => {
      es = new EventSource('/api/v1/events');
      es.onopen = () => setConnected(true);
      es.onerror = () => {
        setConnected(false);
        es?.close();
        if (!closed) retry = window.setTimeout(open, 3000);
      };
      es.addEventListener('snapshot', (ev) => {
        const snap = JSON.parse((ev as MessageEvent).data) as Snapshot;
        setSnapshot(snap);
        const t = Date.now() / 1000;
        setHistory((h) => [...h.slice(-MAX_HISTORY + 1), {
          t, viewers: snap.overview.viewers, in: snap.overview.bitrate_in_kbps, out: snap.overview.bitrate_out_kbps,
        }]);
        const next: Record<string, number[]> = {};
        for (const s of snap.streams) {
          const prev = streamHistory.current[s.path] ?? [];
          next[s.path] = [...prev.slice(-MAX_STREAM_HISTORY + 1), s.bitrate_in_kbps];
        }
        streamHistory.current = next;
      });
      es.addEventListener('event', (ev) => {
        const e = JSON.parse((ev as MessageEvent).data) as ServerEvent;
        setEvents((list) => [e, ...list].slice(0, MAX_EVENTS));
      });
    };
    open();
    return () => {
      closed = true;
      window.clearTimeout(retry);
      es?.close();
    };
  }, []);

  return (
    <LiveContext.Provider value={{ snapshot, connected, events, history, streamHistory: streamHistory.current }}>
      {children}
    </LiveContext.Provider>
  );
}

export function useLive(): LiveValue {
  const v = useContext(LiveContext);
  if (!v) throw new Error('useLive outside LiveProvider');
  return v;
}
