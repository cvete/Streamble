import { Link } from 'react-router-dom';
import { useApi } from '../lib/hooks';
import { useLive } from '../lib/live';
import { ago, bytes } from '../lib/format';
import type { App } from '../lib/types';
import { Empty, PageHead, StateLabel } from '../components/ui';

export function Restreams() {
  const { snapshot } = useLive();
  const apps = useApi<{ apps: App[] }>('/config/apps');
  const pushes = snapshot?.pushes ?? [];
  const targets = (apps.data?.apps ?? []).flatMap((a) => (a.push ?? []).map((p) => ({ app: a.name, ...p })));
  const live = pushes.filter((p) => p.state === 'live').length;

  return (
    <div className="page">
      <PageHead title="Restreams">
        Copies of your streams sent to YouTube, Twitch, Facebook, other servers or Icecast. Targets are set per app in Apps and keys.
      </PageHead>

      <div className="readouts">
        <div className="readout"><div className="v">{live}</div><div className="k">sending now</div></div>
        <div className="readout"><div className="v">{pushes.length - live}</div><div className="k">connecting or retrying</div></div>
        <div className="readout"><div className="v">{targets.length}</div><div className="k">configured targets</div></div>
        <div className="readout"><div className="v">{bytes(pushes.reduce((n, p) => n + p.bytes_sent, 0))}</div><div className="k">sent this session</div></div>
      </div>

      <div className="panel" style={{ marginTop: 16 }}>
        <div className="panel-head"><h3>Active restreams</h3></div>
        <div className="panel-body flush">
          {pushes.length === 0 ? (
            <Empty title="Nothing is being restreamed"><p>A restream starts when a stream goes on air in an app with targets.</p></Empty>
          ) : (
            <table>
              <thead><tr><th>Stream</th><th>Target</th><th>State</th><th>Address</th><th className="num">Sent</th><th className="num">Reconnects</th><th className="num">Since</th></tr></thead>
              <tbody>
                {pushes.map((p) => (
                  <tr key={p.stream + '|' + p.name}>
                    <td><Link className="cell-main" to={'/streams/' + p.stream}>{p.stream}</Link></td>
                    <td>{p.name}</td>
                    <td><StateLabel state={p.state} title={p.last_error} />
                      {p.last_error && <div className="muted" style={{ fontSize: 'var(--t-xs)', maxWidth: 320 }}>{p.last_error}</div>}</td>
                    <td className="mono muted" style={{ maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.url}</td>
                    <td className="num">{bytes(p.bytes_sent)}</td>
                    <td className="num">{p.reconnects}</td>
                    <td className="num muted">{ago(p.since)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <div className="panel" style={{ marginTop: 16 }}>
        <div className="panel-head"><h3>Configured targets</h3></div>
        <div className="panel-body flush">
          {targets.length === 0 ? (
            <p className="muted" style={{ padding: '12px 16px' }}>No targets yet. Open an app in <Link to="/apps">Apps and keys</Link> and add YouTube, Twitch or another server.</p>
          ) : (
            <table>
              <thead><tr><th>App</th><th>Target</th><th>Streams</th><th>Status</th></tr></thead>
              <tbody>
                {targets.map((t, i) => (
                  <tr key={i}>
                    <td><Link className="cell-main" to={'/apps/' + t.app}>{t.app}</Link></td>
                    <td>{t.name || 'unnamed'}</td>
                    <td className="text-2">{t.streams?.length ? t.streams.join(', ') : 'every stream'}</td>
                    <td>{t.disabled ? <StateLabel state="paused" /> : <span className="text-2">Active</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}
