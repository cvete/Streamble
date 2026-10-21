import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Power } from 'lucide-react';
import { useLive } from '../lib/live';
import { useAction, useApi } from '../lib/hooks';
import { useSession } from '../lib/session';
import { post, seg } from '../lib/api';
import { bytes, codecs, dateTime, duration, kbps, number, portOf, serverHost } from '../lib/format';
import type { ServerInfo, StatsPoint, Stream } from '../lib/types';
import { Chart } from '../components/Chart';
import { Player } from '../components/Player';
import { Confirm, CopyField, Empty, PageHead, StateLabel, Tally } from '../components/ui';

export function Streams() {
  const { snapshot } = useLive();
  const [q, setQ] = useState('');
  const all = snapshot?.streams ?? [];
  const sources = all.filter((s) => !s.parent && s.path.includes(q.trim()));
  const byParent = useMemo(() => {
    const m: Record<string, Stream[]> = {};
    for (const s of all) if (s.parent) (m[s.parent] ??= []).push(s);
    return m;
  }, [all]);

  const row = (s: Stream, child = false) => (
    <tr key={s.path}>
      <td>
        <span className="inline" style={{ paddingLeft: child ? 22 : 0 }}>
          <Tally tone={child ? 'ok' : 'on'} />
          <Link to={'/streams/' + s.path} className="cell-main">{child ? s.name : s.path}</Link>
        </span>
      </td>
      <td className="text-2">{child ? 'rendition' : s.protocol.toUpperCase()}{!child && <div className="muted mono" style={{ fontSize: 'var(--t-xs)' }}>{s.remote_addr}</div>}</td>
      <td className="text-2">{codecs(s.tracks)}</td>
      <td className="num">{kbps(s.bitrate_in_kbps)}</td>
      <td className="num">{kbps(s.bitrate_out_kbps)}</td>
      <td className="num">{number(s.viewers)}</td>
      <td className="num mono">{duration(s.uptime_seconds)}</td>
    </tr>
  );

  return (
    <div className="page">
      <PageHead title="Streams" actions={
        <input type="text" placeholder="Filter by name" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter streams" style={{ width: 220 }} />
      }>
        Live streams, with their transcoded renditions beneath them.
      </PageHead>
      <div className="panel">
        {all.length === 0 ? (
          <Empty title="No streams are live"><p>Publish to the server and the stream appears here.</p></Empty>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table>
              <thead><tr>
                <th>Stream</th><th>Source</th><th>Tracks</th>
                <th className="num">In</th><th className="num">Out</th><th className="num">Viewers</th><th className="num">On air</th>
              </tr></thead>
              <tbody>
                {sources.map((s) => [row(s), ...(byParent[s.path] ?? []).map((c) => row(c, true))])}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

const variantLabel: Record<string, string> = {
  abr: 'Adaptive (all renditions)', llhls: 'Low-latency HLS', fmp4: 'HLS fMP4', mpegts: 'HLS MPEG-TS',
};

export function StreamDetail() {
  const { app = '', stream = '' } = useParams();
  const path = app + '/' + stream;
  const { snapshot } = useLive();
  const { can } = useSession();
  const navigate = useNavigate();
  const { run, busy } = useAction();
  const [confirm, setConfirm] = useState(false);
  const [latency, setLatency] = useState<number | null>(null);
  const [variant, setVariant] = useState<string>('');
  const server = useApi<ServerInfo>('/server');
  const stats = useApi<{ points: StatsPoint[] }>(`/stats/series?path=${encodeURIComponent(path)}&from=${Math.floor(Date.now() / 60_000) * 60_000 - 6 * 3600_000}`);

  const s = snapshot?.streams.find((x) => x.path === path);
  if (!snapshot) return <div className="page muted">Connecting…</div>;
  if (!s) {
    return (
      <div className="page">
        <Link to="/streams" className="inline"><ArrowLeft size={15} /> Streams</Link>
        <Empty title={`${path} is not live`}><p>It may have just ended. It will appear again when the publisher reconnects.</p></Empty>
      </div>
    );
  }

  const audioOnly = !(s.tracks ?? []).some((t) => t.type === 'video');
  const options = [...(s.hls.abr ? ['abr'] : []), ...Object.keys(s.hls.variants)];
  const chosen = variant && options.includes(variant) ? variant : options[0];
  const src = chosen === 'abr' ? s.hls.abr : s.hls.variants[chosen];
  const origin = window.location.origin;
  const host = serverHost();
  const rtmpPort = server.data ? portOf(server.data.listeners.rtmp.listen) : '1935';
  const srtPort = server.data ? portOf(server.data.listeners.srt.listen) : '8890';
  const pb = server.data?.playback;
  const pushes = (snapshot.pushes ?? []).filter((p) => p.stream === path);
  const transcode = (snapshot.transcodes ?? []).find((t) => t.stream === path || (t.renditions ?? []).includes(path));
  const points = stats.data?.points ?? [];

  const kick = async () => {
    const ok = await run(() => post(`/streams/${seg(path)}/kick`), s.parent ? 'Transcoder restarting.' : 'Publisher disconnected.');
    setConfirm(false);
    if (ok && !s.parent) navigate('/streams');
  };

  return (
    <div className="page">
      <Link to="/streams" className="inline" style={{ marginBottom: 10, display: 'inline-flex' }}><ArrowLeft size={15} /> Streams</Link>
      <PageHead title={s.path} actions={can('operator') && (
        <button className="btn danger" onClick={() => setConfirm(true)}>
          <Power size={15} /> {s.parent ? 'Restart transcoder' : s.protocol === 'pull' ? 'Reconnect source' : 'Disconnect publisher'}
        </button>
      )}>
        {s.parent ? <>Rendition of <Link to={'/streams/' + s.parent}>{s.parent}</Link></> : <>On air for {duration(s.uptime_seconds)} over {s.protocol.toUpperCase()} from <span className="mono">{s.remote_addr}</span></>}
      </PageHead>

      <div className="grid cols-main">
        <div className="grid" style={{ alignContent: 'start' }}>
          <div className="panel">
            {src ? <Player src={src} onLatency={setLatency} lowLatency={chosen === 'llhls'} audioOnly={audioOnly} /> :
              <div className="panel-body muted">This stream has no codec HLS can carry. Use the MPEG-TS or audio URLs, or add a transcode profile.</div>}
            <div className="panel-body inline" style={{ flexWrap: 'wrap', gap: 14 }}>
              {options.length > 1 && (
                <select value={chosen} onChange={(e) => setVariant(e.target.value)} aria-label="Format" style={{ width: 'auto' }}>
                  {options.map((o) => <option key={o} value={o}>{variantLabel[o] ?? o}</option>)}
                </select>
              )}
              <span className="text-2">Latency {latency != null ? latency.toFixed(1) + ' s' : '–'}</span>
              <span className="text-2">{codecs(s.tracks)}</span>
            </div>
          </div>

          <div className="readouts">
            <div className="readout"><div className="v">{number(s.viewers)}</div><div className="k">viewers</div></div>
            <div className="readout"><div className="v">{kbps(s.bitrate_in_kbps)}</div><div className="k">received</div><div className="sub">{bytes(s.bytes_in)} total</div></div>
            <div className="readout"><div className="v">{kbps(s.bitrate_out_kbps)}</div><div className="k">sent</div><div className="sub">{bytes(s.bytes_out)} total</div></div>
          </div>

          <div className="panel">
            <div className="panel-head"><h3>Viewers, last 6 hours</h3></div>
            <div className="panel-body">
              {points.length > 1 ? (
                <Chart times={points.map((p) => p.ts / 1000)} height={180} series={[
                  { label: 'Viewers', color: '#8fb6ff', values: points.map((p) => p.viewers), fill: true },
                  { label: 'Peak', color: '#f0a33a', values: points.map((p) => p.viewers_max) },
                ]} />
              ) : <p className="muted">History builds up a minute at a time while the stream is live.</p>}
            </div>
          </div>
        </div>

        <div className="grid" style={{ alignContent: 'start' }}>
          <div className="panel">
            <div className="panel-head"><h3>Watch and listen</h3></div>
            <div className="panel-body copy-list">
              {s.hls.abr && <CopyField label="HLS, adaptive" value={origin + s.hls.abr} />}
              {s.hls.primary && <CopyField label="HLS" value={origin + s.hls.primary} />}
              {Object.entries(s.hls.variants).filter(([v]) => v !== server.data?.hls.primary).map(([v, u]) => (
                <CopyField key={v} label={variantLabel[v] ?? v} value={origin + u} />
              ))}
              {s.http.ts && <CopyField label="MPEG-TS over HTTP (IPTV, VLC)" value={origin + s.http.ts} />}
              {s.http.mp3 && <CopyField label="MP3 radio stream" value={origin + s.http.mp3} />}
              {s.http.aac && <CopyField label="AAC radio stream" value={origin + s.http.aac} />}
              {pb?.rtmp && <CopyField label="RTMP" value={`rtmp://${host}${rtmpPort === '1935' ? '' : ':' + rtmpPort}/${s.path}`} />}
              {pb?.srt && <CopyField label="SRT" value={`srt://${host}:${srtPort}?streamid=read:${s.path}`} />}
            </div>
          </div>

          {(s.renditions || transcode) && (
            <div className="panel">
              <div className="panel-head"><h3>Transcoding</h3>{transcode && <span className="aside"><StateLabel state={transcode.state} /></span>}</div>
              <div className="panel-body">
                {transcode?.last_error && <p className="muted" style={{ fontSize: 'var(--t-xs)', marginBottom: 8 }}>{transcode.last_error}</p>}
                <dl className="kv">
                  <dt>Profile</dt><dd>{transcode ? <Link to="/transcoding">{transcode.profile}</Link> : '—'}</dd>
                  <dt>Renditions</dt>
                  <dd>{(transcode?.renditions ?? s.renditions ?? []).map((r) => <div key={r}><Link to={'/streams/' + r}>{r}</Link></div>)}</dd>
                  {transcode && <><dt>Restarts</dt><dd>{transcode.restarts}</dd></>}
                </dl>
              </div>
            </div>
          )}

          <div className="panel">
            <div className="panel-head"><h3>Restreams</h3><span className="aside"><Link to={'/apps/' + s.app}>Edit targets</Link></span></div>
            <div className="panel-body flush">
              {pushes.length === 0 ? <p className="muted" style={{ padding: '12px 16px' }}>Not restreamed anywhere.</p> : (
                <table><tbody>{pushes.map((p) => (
                  <tr key={p.name}>
                    <td><div className="cell-main">{p.name}</div><div className="muted mono" style={{ fontSize: 'var(--t-xs)' }}>{p.url}</div>
                      {p.last_error && p.state !== 'live' && <div className="muted" style={{ fontSize: 'var(--t-xs)' }}>{p.last_error}</div>}</td>
                    <td className="num"><StateLabel state={p.state} /></td>
                  </tr>
                ))}</tbody></table>
              )}
            </div>
          </div>

          <div className="panel">
            <div className="panel-head"><h3>Details</h3></div>
            <div className="panel-body">
              <dl className="kv">
                <dt>Started</dt><dd>{dateTime(s.started_at)}</dd>
                <dt>Protocol</dt><dd>{s.protocol}</dd>
                <dt>From</dt><dd className="mono">{s.remote_addr || '—'}</dd>
                {(s.tracks ?? []).flatMap((t, i) => [
                  <dt key={'k' + i}>Track {i + 1}</dt>,
                  <dd key={'v' + i}>{t.codec} {t.type}</dd>,
                ])}
              </dl>
            </div>
          </div>
        </div>
      </div>

      {confirm && (
        <Confirm title={s.parent ? 'Restart the transcoder?' : 'Disconnect the publisher?'} action={s.parent ? 'Restart' : 'Disconnect'}
          busy={busy} onConfirm={kick} onClose={() => setConfirm(false)}>
          {s.parent
            ? 'All renditions of this stream stop for a few seconds while FFmpeg restarts.'
            : s.protocol === 'pull'
              ? 'The pull source drops its connection and reconnects. Viewers see a short interruption.'
              : 'The encoder is disconnected and viewers lose the picture. Most encoders reconnect on their own; to keep one out, change its key first.'}
        </Confirm>
      )}
    </div>
  );
}
