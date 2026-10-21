import { useEffect, useRef, useState } from 'react';
import type Hls from 'hls.js';

// hls.js is large; load it only when a player is shown.
let hlsModule: Promise<typeof import('hls.js')> | null = null;
const loadHls = () => (hlsModule ??= import('hls.js'));

/** HLS player: hls.js where supported, native HLS on Safari. */
export function Player({ src, muted = false, controls = true, lowLatency = true, onLatency, audioOnly }: {
  src: string;
  muted?: boolean;
  controls?: boolean;
  lowLatency?: boolean;
  onLatency?: (seconds: number | null) => void;
  audioOnly?: boolean;
}) {
  const video = useRef<HTMLVideoElement>(null);
  const [problem, setProblem] = useState<string | null>(null);

  useEffect(() => {
    const el = video.current;
    if (!el) return;
    setProblem(null);
    let hls: Hls | null = null;
    let retry: number | undefined;
    let timer: number | undefined;
    let cancelled = false;
    let Hls: typeof import('hls.js').default;

    const start = () => {
      if (cancelled) return;
      if (Hls.isSupported()) {
        hls = new Hls({ lowLatencyMode: lowLatency, backBufferLength: 30, maxBufferLength: 10 });
        hls.loadSource(src);
        hls.attachMedia(el);
        hls.on(Hls.Events.ERROR, (_e, data) => {
          if (!data.fatal) return;
          setProblem('The stream stopped or cannot be played. Retrying…');
          hls?.destroy();
          retry = window.setTimeout(start, 3000);
        });
        hls.on(Hls.Events.FRAG_LOADED, () => setProblem(null));
        if (onLatency) {
          timer = window.setInterval(() => onLatency(hls && Number.isFinite(hls.latency) ? hls.latency : null), 1000);
        }
      } else if (el.canPlayType('application/vnd.apple.mpegurl')) {
        el.src = src;
      } else {
        setProblem('This browser cannot play HLS.');
        return;
      }
      el.play().catch(() => {
        /* autoplay with sound may be blocked; the controls let the user start it */
      });
    };
    loadHls().then((m) => {
      Hls = m.default;
      start();
    }, () => setProblem('The player could not be loaded.'));
    return () => {
      cancelled = true;
      window.clearTimeout(retry);
      window.clearInterval(timer);
      hls?.destroy();
      el.removeAttribute('src');
      el.load();
    };
  }, [src, lowLatency, onLatency]);

  return (
    <div className={'player' + (audioOnly ? ' audio' : '')}>
      <video ref={video} muted={muted} controls={controls} playsInline />
      {problem && <div className="signal"><div className="small">{problem}</div></div>}
    </div>
  );
}
