import { useState, type FormEvent } from 'react';
import { Plus, Trash2 } from 'lucide-react';
import { del, post, put } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { useSession } from '../lib/session';
import { ago, duration } from '../lib/format';
import type { APIToken, Playback, ServerInfo } from '../lib/types';
import { Confirm, CopyField, Dialog, ErrorNotice, Field, PageHead, Switch } from '../components/ui';
import { ChangePassword } from './Login';

const playbackHelp: { key: keyof Playback; label: string; hint: string }[] = [
  { key: 'http_ts', label: 'MPEG-TS over HTTP', hint: '/app/stream.ts for set-top boxes, VLC and IPTV players.' },
  { key: 'http_audio', label: 'Radio over HTTP', hint: '/app/stream.mp3 and .aac for audio streams, like Icecast.' },
  { key: 'rtmp', label: 'RTMP playback', hint: 'Lets other servers and players pull streams over RTMP.' },
  { key: 'srt', label: 'SRT playback', hint: 'Lets receivers pull streams over SRT with streamid=read:app/stream.' },
];

export function Settings() {
  const { data, error, reload } = useApi<ServerInfo>('/server');
  const { can } = useSession();
  const { run, busy } = useAction();
  const admin = can('admin');

  const setPlayback = (key: keyof Playback, v: boolean) => {
    if (!data) return;
    run(() => put('/config/playback', { ...data.playback, [key]: v }).then(reload), 'Playback updated.');
  };

  return (
    <div className="page">
      <PageHead title="Settings">
        Listeners, HLS and storage come from the server config file and need a restart. Everything else in this panel applies immediately.
      </PageHead>
      <ErrorNotice error={error} />
      {data && (
        <div className="grid cols-2">
          <div className="panel">
            <div className="panel-head"><h3>Playback</h3>{!admin && <span className="aside">admins can change these</span>}</div>
            <div className="panel-body fields">
              <p className="text-2">HLS and low-latency HLS are always on. These add other ways to watch.</p>
              {playbackHelp.map((p) => (
                <div key={p.key} className="setting">
                  <Switch checked={data.playback[p.key]} disabled={!admin || busy} onChange={(v) => setPlayback(p.key, v)} label={p.label} />
                  <div className="muted">{p.hint}</div>
                </div>
              ))}
            </div>
          </div>
          <div className="panel">
            <div className="panel-head"><h3>Server</h3><span className="aside">from the config file</span></div>
            <div className="panel-body">
              <dl className="kv">
                <dt>Version</dt><dd>{data.version}</dd>
                <dt>Running for</dt><dd>{duration(data.uptime_seconds)}</dd>
                <dt>Storage</dt><dd>{data.storage}</dd>
                <dt>RTMP</dt><dd>{data.listeners.rtmp.enabled ? <code>{data.listeners.rtmp.listen}</code> : 'off'}</dd>
                <dt>SRT</dt><dd>{data.listeners.srt.enabled ? <><code>{data.listeners.srt.listen}</code>, {data.listeners.srt.latency_ms} ms latency</> : 'off'}</dd>
                <dt>HTTP</dt><dd><code>{data.listeners.http.listen}</code></dd>
                <dt>HLS</dt><dd>{data.hls.variants.join(', ')}; {data.hls.segment_count} segments of {data.hls.segment_duration}, parts of {data.hls.part_duration}</dd>
                <dt>FFmpeg</dt><dd><code>{data.transcode.ffmpeg}</code></dd>
                <dt>Countries</dt><dd>{data.geoip.enabled ? 'GeoLite2 database loaded' : 'off (no GeoLite2 database)'}</dd>
                <dt>Statistics kept</dt><dd>{data.stats.retention_days} days</dd>
                <dt>Source code</dt><dd><a href={data.source} target="_blank" rel="noreferrer">{data.source}</a> (AGPL-3.0)</dd>
              </dl>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function Tokens() {
  const { data, error, reload } = useApi<{ tokens: APIToken[] | null }>('/auth/tokens');
  const { run, busy } = useAction();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const [created, setCreated] = useState<string | null>(null);
  const [createError, setCreateError] = useState<unknown>(null);
  const [revoking, setRevoking] = useState<APIToken | null>(null);
  const tokens = data?.tokens ?? [];

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setCreateError(null);
    try {
      const res = await post<{ token: string }>('/auth/tokens', { name });
      setCreated(res.token);
      setName('');
      reload();
    } catch (err) {
      setCreateError(err);
    }
  };

  return (
    <div className="panel">
      <div className="panel-head">
        <h3>API tokens</h3>
        <button className="btn small" onClick={() => { setCreated(null); setCreating(true); }}><Plus size={13} /> New token</button>
      </div>
      <div className="panel-body flush">
        <ErrorNotice error={error} />
        {tokens.length === 0 ? (
          <p className="muted" style={{ padding: '12px 16px' }}>
            Tokens let scripts use the API with your role: <code>Authorization: Bearer stb_…</code>
          </p>
        ) : (
          <table>
            <thead><tr><th>Name</th><th>Token</th><th className="num">Created</th><th className="num">Last used</th><th /></tr></thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td className="cell-main">{t.name}</td>
                  <td className="mono muted">{t.prefix}…</td>
                  <td className="num muted">{ago(t.created_at)}</td>
                  <td className="num muted">{t.last_used_at ? ago(t.last_used_at) : 'never'}</td>
                  <td className="num"><button className="btn ghost small icon" aria-label={'Revoke ' + t.name} onClick={() => setRevoking(t)}><Trash2 size={13} /></button></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {creating && (
        <Dialog title={created ? 'Token created' : 'New API token'} onClose={() => setCreating(false)} footer={created
          ? <button className="btn primary" onClick={() => setCreating(false)}>Done</button>
          : <><button className="btn" onClick={() => setCreating(false)}>Cancel</button><button className="btn primary" form="token-form">Create token</button></>}>
          {created ? (
            <div className="fields">
              <p className="text-2">Copy it now. It is not shown again.</p>
              <CopyField label="Token" value={created} />
            </div>
          ) : (
            <form id="token-form" className="fields" onSubmit={submit}>
              <Field label="Name" hint="What uses it, e.g. scheduler or monitoring.">
                <input type="text" value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
              </Field>
              <ErrorNotice error={createError} />
            </form>
          )}
        </Dialog>
      )}
      {revoking && (
        <Confirm title={`Revoke ${revoking.name}?`} action="Revoke token" busy={busy} onClose={() => setRevoking(null)} onConfirm={async () => {
          if (await run(() => del('/auth/tokens/' + revoking.id), 'Token revoked.')) {
            setRevoking(null);
            reload();
          }
        }}>Scripts using this token stop working immediately.</Confirm>
      )}
    </div>
  );
}

export function Account() {
  const { user } = useSession();
  return (
    <div className="page">
      <PageHead title="Account">Signed in as {user?.username}, {user?.role}.</PageHead>
      <div className="grid cols-2">
        <div className="panel">
          <div className="panel-head"><h3>Password</h3></div>
          <div className="panel-body"><ChangePassword /></div>
        </div>
        <Tokens />
      </div>
    </div>
  );
}
