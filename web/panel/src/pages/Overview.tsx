import { Link } from 'react-router-dom';
import { useLive } from '../lib/live';
import { useApi, useStored } from '../lib/hooks';
import { codecs, duration, kbps, number, portOf, serverHost } from '../lib/format';
import type { ServerEvent, ServerInfo, Stream } from '../lib/types';
import { Chart } from '../components/Chart';
import { Player } from '../components/Player';
import { CopyField, Empty, Meter, PageHead, StateLabel, Switch, Tally } from '../components/ui';

const MAX_PICTURES = 9;

function Tile({ s, picture, history }: { s: Stream; picture: boolean; history: number[] }) {
  const audioOnly = !(s.tracks ?? []).some((t) => t.type === 'video');
  const url = s.hls.abr ?? s.hls.primary;
  return (
    <div className="tile">
      <div className="screen">
        {picture && url && !audioOnly ? (
          <Player src={url} muted controls={false} />
        ) : (
          <div className="signal">
            <div>
              <div className="big">{number(s.viewers)}</div>
              <div className="small">{s.viewers === 1 ? 'viewer' : 'viewers'} · {codecs(s.tracks)}</div>
            </div>
          </div>
        )}
        <div className="screen-badges">
          <span className="chip onair">On air</span>
          <span className="chip">{s.protocol.toUpperCase()}</span>
          {audioOnly && <span className="chip">Radio</span>}
          {s.renditions && <span className="chip">{s.renditions.length} {s.renditions.length === 1 ? 'rendition' : 'renditions'}</span>}
        </div>
      </div>
      <div className="umd">
        <Tally tone="on" />
        <span className="name"><Link to={'/streams/' + s.path}>{s.path}</Link></span>
        <span className="meta">
          <Meter values={history} />
          <span>{kbps(s.bitrate_in_kbps)}</span>
          <span className="mono">{duration(s.uptime_seconds)}</span>
        </span>
      </div>
    </div>
  );
}

function describe(e: ServerEvent): { tone: 'on' | 'ok' | 'warn' | 'bad' | 'off'; text: string } {
  const d = e.data as Record<string, any>;
  switch (e.type) {
    case 'stream.published':
      return { tone: 'on', text: `${d.path} went on air over ${String(d.protocol).toUpperCase()}` };
    case 'stream.ended':
      return { tone: 'off', text: `${d.path} went off air after ${duration(d.duration_seconds ?? 0)}` };
    case 'push.state':
      return { tone: d.state === 'live' ? 'ok' : d.state === 'failed' ? 'bad' : 'warn', text: `Restream ${d.name} of ${d.stream}: ${d.state}` };
    case 'source.state':
      return { tone: d.state === 'live' ? 'ok' : 'warn', text: `Pull source ${d.path}: ${d.state}` };
    case 'transcode.state':
      return { tone: d.state === 'running' ? 'ok' : 'warn', text: `Transcoder for ${d.stream}: ${d.state}` };
  }
  return { tone: 'off', text: e.type };
}

function GettingStarted() {
  const { data } = useApi<ServerInfo>('/server');
  const host = serverHost();
  const rtmpPort = data ? portOf(data.listeners.rtmp.listen) : '1935';
  const srtPort = data ? portOf(data.listeners.srt.listen) : '8890';
  return (
    <div className="panel">
      <Empty title="Nothing is on air">
        <p>Point an encoder at this server. Streams appear here as soon as they arrive. Add a publish key in Apps and keys and append it as <code>?key=…</code>.</p>
      </Empty>
      <div className="panel-body copy-list" style={{ maxWidth: 680, margin: '0 auto', paddingTop: 0 }}>
        <CopyField label="OBS or vMix: server (stream key: the stream name, e.g. tv1)" value={`rtmp://${host}${rtmpPort === '1935' ? '' : ':' + rtmpPort}/live`} />
        <CopyField label="SRT encoder (caller mode)" value={`srt://${host}:${srtPort}?streamid=live/tv1`} />
      </div>
    </div>
  );
}

export function Overview() {
  const { snapshot, events, history, streamHistory } = useLive();
  const [pictures, setPictures] = useStored('wall.pictures', true);
  const streams = (snapshot?.streams ?? []).filter((s) => !s.parent);
  const o = snapshot?.overview;
  const transcodes = snapshot?.transcodes ?? [];
  const pushes = snapshot?.pushes ?? [];
  const sources = snapshot?.sources ?? [];

  const problems = [
    ...pushes.filter((p) => p.state !== 'live').map((p) => ({ key: 'p' + p.stream + p.name, state: p.state, what: `Restream ${p.name}`, of: p.stream, err: p.last_error, to: '/restreams' })),
    ...sources.filter((s) => s.state !== 'live').map((s) => ({ key: 's' + s.path, state: s.state, what: 'Pull source', of: s.path, err: s.last_error, to: '/sources' })),
    ...transcodes.filter((t) => t.state !== 'running').map((t) => ({ key: 't' + t.stream, state: t.state, what: 'Transcoder', of: t.stream, err: t.last_error, to: '/transcoding' })),
  ];

  const times = history.map((h) => h.t);

  return (
    <div className="page">
      <PageHead title="Overview" actions={
        <Switch checked={pictures} onChange={setPictures} label={`Live pictures${streams.length > MAX_PICTURES ? ` (first ${MAX_PICTURES})` : ''}`} />
      }>
        Every channel on air, what is sent out, and anything that needs attention.
      </PageHead>

      {!snapshot ? (
        <div className="panel"><div className="panel-body muted">Connecting to the server…</div></div>
      ) : streams.length === 0 ? (
        <GettingStarted />
      ) : (
        <div className="wall" aria-label="Channels on air">
          {streams.map((s, i) => (
            <Tile key={s.path} s={s} picture={pictures && i < MAX_PICTURES} history={streamHistory[s.path] ?? []} />
          ))}
        </div>
      )}

      <div className="readouts" style={{ marginTop: 16 }}>
        <div className="readout"><div className="v">{o ? number(o.viewers) : '–'}</div><div className="k">viewers now</div></div>
        <div className="readout"><div className="v">{o ? kbps(o.bitrate_out_kbps) : '–'}</div><div className="k">sent to viewers</div></div>
        <div className="readout"><div className="v">{o ? kbps(o.bitrate_in_kbps) : '–'}</div><div className="k">received</div></div>
        <div className="readout">
          <div className="v">{transcodes.filter((t) => t.state === 'running').length}/{transcodes.length}</div>
          <div className="k">transcoders running</div>
        </div>
        <div className="readout">
          <div className="v">{pushes.filter((p) => p.state === 'live').length}/{pushes.length}</div>
          <div className="k">restreams live</div>
        </div>
        <div className="readout">
          <div className="v">{sources.filter((s) => s.state === 'live').length}/{sources.length}</div>
          <div className="k">pull sources live</div>
        </div>
      </div>

      <div className="grid cols-main" style={{ marginTop: 16 }}>
        <div className="panel">
          <div className="panel-head"><h3>Last 15 minutes</h3><span className="aside">since this page opened</span></div>
          <div className="panel-body">
            {history.length > 1 ? (
              <Chart times={times} height={200} yFormat={(v) => kbps(v)} series={[
                { label: 'Sent', color: '#8fb6ff', values: history.map((h) => h.out), fill: true, format: kbps },
                { label: 'Received', color: '#46a758', values: history.map((h) => h.in), format: kbps },
              ]} />
            ) : <p className="muted">Collecting…</p>}
          </div>
        </div>
        <div className="grid">
          <div className="panel">
            <div className="panel-head"><h3>Needs attention</h3><span className="aside">{problems.length || 'all clear'}</span></div>
            <div className="panel-body flush">
              {problems.length === 0 ? (
                <p className="muted" style={{ padding: '12px 16px' }}>Every transcoder, restream and pull source is running.</p>
              ) : (
                <table><tbody>
                  {problems.slice(0, 6).map((p) => (
                    <tr key={p.key}>
                      <td><StateLabel state={p.state} /></td>
                      <td><Link to={p.to}>{p.what}</Link> <span className="muted">{p.of}</span>
                        {p.err && <div className="muted" style={{ fontSize: 'var(--t-xs)' }}>{p.err}</div>}</td>
                    </tr>
                  ))}
                </tbody></table>
              )}
            </div>
          </div>
          <div className="panel">
            <div className="panel-head"><h3>Activity</h3></div>
            <div className="panel-body flush" style={{ maxHeight: 260, overflowY: 'auto' }}>
              {events.length === 0 ? (
                <p className="muted" style={{ padding: '12px 16px' }}>Streams starting and stopping, restreams and sources changing state appear here.</p>
              ) : (
                <ul className="feed">
                  {events.map((e, i) => {
                    const d = describe(e);
                    return (
                      <li key={i}>
                        <time dateTime={e.at}>{new Date(e.at).toLocaleTimeString('en-GB', { hour12: false })}</time>
                        <Tally tone={d.tone} />
                        <span>{d.text}</span>
                      </li>
                    );
                  })}
                </ul>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
