import { useEffect, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, Plus, Trash2 } from 'lucide-react';
import { del, post, put, seg } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { portOf, randomKey, serverHost } from '../lib/format';
import type { App, Profile, Push, ServerInfo } from '../lib/types';
import { Confirm, CopyField, Dialog, Empty, ErrorNotice, Field, PageHead, SecretField, StateLabel, Switch } from '../components/ui';

const blankApp = (): App => ({
  name: '', description: '', publish_key: randomKey(), stream_keys: {}, srt_passphrase: '',
  play_key: '', auth_url: '', transcode: '', push: [],
});

export function Apps() {
  const { data, error, reload } = useApi<{ apps: App[] }>('/config/apps');
  const { snapshot } = useLive();
  const { can } = useSession();
  const [creating, setCreating] = useState(false);
  const apps = data?.apps ?? [];
  const live = (name: string) => (snapshot?.streams ?? []).filter((s) => s.app === name && !s.parent).length;

  return (
    <div className="page">
      <PageHead title="Apps and keys" actions={can('operator') && (
        <button className="btn primary" onClick={() => setCreating(true)}><Plus size={15} /> New app</button>
      )}>
        An app groups streams under one address, like <code>live</code> in <code>rtmp://server/live/tv1</code>, and holds their keys, transcoding and restream targets.
      </PageHead>
      <ErrorNotice error={error} />
      <div className="panel">
        {apps.length === 0 && data ? (
          <Empty title="No apps yet"><p>Create an app to start receiving streams.</p></Empty>
        ) : (
          <table>
            <thead><tr><th>App</th><th>Publishing</th><th>Transcoding</th><th>Restream targets</th><th className="num">Live streams</th></tr></thead>
            <tbody>
              {apps.map((a) => (
                <tr key={a.name}>
                  <td><Link to={'/apps/' + a.name} className="cell-main">{a.name}</Link>
                    {a.description && <div className="muted" style={{ fontSize: 'var(--t-xs)' }}>{a.description}</div>}</td>
                  <td className="text-2">{a.publish_key || Object.keys(a.stream_keys ?? {}).length ? 'Key required' : <span style={{ color: 'var(--warn)' }}>Open to anyone</span>}
                    {a.srt_passphrase && ', SRT encrypted'}{a.auth_url && ', auth callback'}</td>
                  <td className="text-2">{a.transcode || '—'}</td>
                  <td className="text-2">{(a.push ?? []).length ? (a.push ?? []).map((p) => p.name || 'unnamed').join(', ') : '—'}</td>
                  <td className="num">{live(a.name)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {creating && <NewApp onClose={() => setCreating(false)} onCreated={reload} />}
    </div>
  );
}

function NewApp({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [app, setApp] = useState<App>(blankApp);
  const [error, setError] = useState<unknown>(null);
  const navigate = useNavigate();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await post('/config/apps', app);
      onCreated();
      navigate('/apps/' + app.name);
    } catch (err) {
      setError(err);
    }
  };
  return (
    <Dialog title="New app" onClose={onClose} footer={
      <><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" form="new-app">Create app</button></>
    }>
      <form id="new-app" className="fields" onSubmit={submit}>
        <Field label="Name" hint="Letters, digits, '-', '_' and '.'. It becomes part of every stream address and cannot be changed later.">
          <input type="text" value={app.name} onChange={(e) => setApp({ ...app, name: e.target.value })} required pattern="[A-Za-z0-9_\-][A-Za-z0-9_.\-]*" />
        </Field>
        <Field label="Description">
          <input type="text" value={app.description} onChange={(e) => setApp({ ...app, description: e.target.value })} placeholder="e.g. Main TV channels" />
        </Field>
        <SecretField label="Publish key" value={app.publish_key} onChange={(v) => setApp({ ...app, publish_key: v })}
          hint="Encoders must send this key. Leave empty to let anyone publish (not recommended on the internet)." />
        <ErrorNotice error={error} />
      </form>
    </Dialog>
  );
}

const presets: { label: string; name: string; url: string; hint: string }[] = [
  { label: 'YouTube', name: 'youtube', url: 'rtmp://a.rtmp.youtube.com/live2/', hint: 'Paste your YouTube stream key at the end.' },
  { label: 'Twitch', name: 'twitch', url: 'rtmp://live.twitch.tv/app/', hint: 'Paste your Twitch stream key at the end.' },
  { label: 'Facebook', name: 'facebook', url: 'rtmps://live-api-s.facebook.com:443/rtmp/', hint: 'Paste your Facebook stream key at the end.' },
  { label: 'RTMP server', name: 'rtmp', url: 'rtmp://host/app/{stream}', hint: 'Another server. {stream} is replaced with the stream name.' },
  { label: 'SRT', name: 'srt', url: 'srt://host:9000?streamid=live/{stream}', hint: 'Add &passphrase=... if the receiver needs one.' },
  { label: 'Icecast', name: 'icecast', url: 'icecast://source:PASSWORD@host:8000/{stream}.mp3', hint: 'Needs MP3 or AAC audio, e.g. from an audio rendition.' },
];

function PushEditor({ pushes, onChange, profiles, transcode }: {
  pushes: Push[]; onChange: (p: Push[]) => void; profiles: Profile[]; transcode: string;
}) {
  const [hint, setHint] = useState<string[]>([]);
  const set = (i: number, patch: Partial<Push>) => onChange(pushes.map((p, j) => (j === i ? { ...p, ...patch } : p)));
  const rendNames = profiles.find((p) => p.name === transcode)?.renditions.map((r) => r.name) ?? [];
  return (
    <div className="fields">
      {pushes.length === 0 && <p className="muted">Streams of this app are not sent anywhere else.</p>}
      {pushes.map((p, i) => (
        <div key={i} className="panel" style={{ background: 'var(--panel-2)' }}>
          <div className="panel-body fields">
            <div className="fields two">
              <Field label="Name"><input type="text" value={p.name} onChange={(e) => set(i, { name: e.target.value })} /></Field>
              <Field label="Only these streams" hint={rendNames.length ? `Comma-separated. Renditions must be listed by name, e.g. tv1_${rendNames[0]}.` : 'Comma-separated stream names; empty for every stream.'}>
                <input type="text" value={(p.streams ?? []).join(', ')} placeholder="every stream"
                  onChange={(e) => set(i, { streams: e.target.value.split(',').map((x) => x.trim()).filter(Boolean) })} />
              </Field>
            </div>
            <Field label="Target URL" hint={hint[i]}>
              <input type="text" className="mono" value={p.url} onChange={(e) => set(i, { url: e.target.value })} spellCheck={false} />
            </Field>
            <div className="inline" style={{ justifyContent: 'space-between' }}>
              <Switch checked={!p.disabled} onChange={(v) => set(i, { disabled: !v })} label={p.disabled ? 'Paused' : 'Active'} />
              <button type="button" className="btn ghost small" onClick={() => onChange(pushes.filter((_, j) => j !== i))}><Trash2 size={13} /> Remove</button>
            </div>
          </div>
        </div>
      ))}
      <div className="inline" style={{ flexWrap: 'wrap' }}>
        <span className="muted">Add:</span>
        {presets.map((pr) => (
          <button key={pr.label} type="button" className="btn small" onClick={() => {
            onChange([...pushes, { name: pr.name, url: pr.url, streams: [], disabled: false }]);
            setHint([...hint.slice(0, pushes.length), pr.hint]);
          }}>{pr.label}</button>
        ))}
      </div>
    </div>
  );
}

function StreamKeys({ keys, onChange }: { keys: Record<string, string>; onChange: (k: Record<string, string>) => void }) {
  const entries = Object.entries(keys);
  const [name, setName] = useState('');
  return (
    <div className="fields">
      {entries.length === 0 && <p className="muted">All streams use the app's publish key.</p>}
      {entries.map(([n, k]) => (
        <div key={n} className="inline">
          <code style={{ minWidth: 120 }}>{n}</code>
          <SecretField label="" value={k} onChange={(v) => onChange({ ...keys, [n]: v })} />
          <button type="button" className="btn ghost icon" aria-label={'Remove key of ' + n} onClick={() => {
            const next = { ...keys };
            delete next[n];
            onChange(next);
          }}><Trash2 size={14} /></button>
        </div>
      ))}
      <div className="inline">
        <input type="text" placeholder="stream name, e.g. tv1" value={name} onChange={(e) => setName(e.target.value)} aria-label="Stream name" />
        <button type="button" className="btn" disabled={!name.trim()} onClick={() => { onChange({ ...keys, [name.trim()]: randomKey() }); setName(''); }}>
          <Plus size={14} /> Dedicated key
        </button>
      </div>
    </div>
  );
}

function EncoderSetup({ app }: { app: App }) {
  const server = useApi<ServerInfo>('/server');
  const [stream, setStream] = useState('tv1');
  const host = serverHost();
  const rtmpPort = server.data ? portOf(server.data.listeners.rtmp.listen) : '1935';
  const srtPort = server.data ? portOf(server.data.listeners.srt.listen) : '8890';
  const key = app.stream_keys?.[stream] ?? app.publish_key;
  const masked = key === '********';
  const q = key && !masked ? `?key=${key}` : '';
  return (
    <div className="fields">
      <Field label="Stream name"><input type="text" value={stream} onChange={(e) => setStream(e.target.value.replace(/[^A-Za-z0-9_.-]/g, ''))} /></Field>
      {masked && <div className="notice">Keys are hidden for your role; ask an operator for the full addresses.</div>}
      <div className="copy-list">
        <CopyField label="OBS, vMix: server" value={`rtmp://${host}${rtmpPort === '1935' ? '' : ':' + rtmpPort}/${app.name}`} />
        <CopyField label="OBS, vMix: stream key" value={stream + q} />
        <CopyField label="SRT encoder (caller mode)" value={`srt://${host}:${srtPort}?streamid=${app.name}/${stream}${q}${app.srt_passphrase && !masked ? `&passphrase=${app.srt_passphrase}` : ''}`} />
        <CopyField label="Watch (HLS)" value={`${window.location.origin}/${app.name}/${stream}/index.m3u8`} />
      </div>
    </div>
  );
}

export function AppEditor() {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const { can } = useSession();
  const { snapshot } = useLive();
  const loaded = useApi<{ app: App }>('/config/apps/' + seg(name));
  const profiles = useApi<{ profiles: Profile[] }>('/config/profiles');
  const [app, setApp] = useState<App | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const { run, busy } = useAction();
  const readOnly = !can('operator');

  useEffect(() => {
    if (loaded.data) setApp(loaded.data.app);
  }, [loaded.data]);

  if (loaded.error) return <div className="page"><ErrorNotice error={loaded.error} /></div>;
  if (!app) return <div className="page muted">Loading…</div>;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      const res = await put<{ app: App }>('/config/apps/' + seg(name), app);
      setApp(res.app);
      loaded.setData(res);
      await run(async () => {}, 'Saved. Changes apply to live streams now.');
    } catch (err) {
      setError(err);
    }
  };
  const remove = async () => {
    if (await run(() => del('/config/apps/' + seg(name)), `App ${name} deleted.`)) navigate('/apps');
  };
  const pushStatus = (snapshot?.pushes ?? []).filter((p) => p.stream.startsWith(name + '/'));

  return (
    <div className="page">
      <Link to="/apps" className="inline" style={{ marginBottom: 10, display: 'inline-flex' }}><ArrowLeft size={15} /> Apps and keys</Link>
      <PageHead title={name} actions={!readOnly && (
        <>
          <button className="btn danger" type="button" onClick={() => setConfirmDelete(true)}><Trash2 size={15} /> Delete</button>
          <button className="btn primary" form="app-form" disabled={busy}>Save changes</button>
        </>
      )}>
        {app.description || 'Settings for every stream published under ' + name + '/.'}
      </PageHead>
      <ErrorNotice error={error} />
      <form id="app-form" onSubmit={save} className="grid cols-2" style={{ marginTop: error ? 12 : 0 }}>
        <fieldset disabled={readOnly} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} className="grid">
          <div className="panel">
            <div className="panel-head"><h3>General</h3></div>
            <div className="panel-body fields">
              <Field label="Description"><input type="text" value={app.description} onChange={(e) => setApp({ ...app, description: e.target.value })} /></Field>
              <Field label="Transcoding profile" hint={<>Every stream of this app gets these renditions. <Link to="/transcoding">Manage profiles</Link></>}>
                <select value={app.transcode} onChange={(e) => setApp({ ...app, transcode: e.target.value })}>
                  <option value="">None, deliver as received</option>
                  {(profiles.data?.profiles ?? []).map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
                </select>
              </Field>
            </div>
          </div>
          <div className="panel">
            <div className="panel-head"><h3>Who can publish and watch</h3></div>
            <div className="panel-body fields">
              <SecretField label="Publish key" value={app.publish_key} onChange={(v) => setApp({ ...app, publish_key: v })}
                hint="Sent by encoders as ?key=… after the stream name. Empty: anyone can publish." />
              <SecretField label="SRT passphrase" value={app.srt_passphrase} onChange={(v) => setApp({ ...app, srt_passphrase: v })}
                hint="Encrypts SRT publishing and playback (10 to 79 characters)." minLength={10} />
              <SecretField label="Play key" value={app.play_key} onChange={(v) => setApp({ ...app, play_key: v })}
                hint="Required for RTMP, SRT, .ts and radio playback. HLS stays open." />
              <Field label="Auth callback URL" hint="Optional. Every publish and play is checked by this URL; anything but 2xx refuses it.">
                <input type="url" className="mono" value={app.auth_url} placeholder="https://…" onChange={(e) => setApp({ ...app, auth_url: e.target.value })} />
              </Field>
            </div>
          </div>
          <div className="panel">
            <div className="panel-head"><h3>Dedicated stream keys</h3></div>
            <div className="panel-body">
              <StreamKeys keys={app.stream_keys ?? {}} onChange={(k) => setApp({ ...app, stream_keys: k })} />
            </div>
          </div>
        </fieldset>
        <div className="grid" style={{ alignContent: 'start' }}>
          <fieldset disabled={readOnly} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
            <div className="panel">
              <div className="panel-head"><h3>Restream targets</h3></div>
              <div className="panel-body">
                <PushEditor pushes={app.push ?? []} onChange={(p) => setApp({ ...app, push: p })}
                  profiles={profiles.data?.profiles ?? []} transcode={app.transcode} />
                {pushStatus.length > 0 && (
                  <table style={{ marginTop: 14 }}><tbody>{pushStatus.map((p) => (
                    <tr key={p.stream + p.name}><td>{p.stream} → {p.name}</td><td className="num"><StateLabel state={p.state} title={p.last_error} /></td></tr>
                  ))}</tbody></table>
                )}
              </div>
            </div>
          </fieldset>
          <div className="panel">
            <div className="panel-head"><h3>Encoder setup</h3></div>
            <div className="panel-body"><EncoderSetup app={app} /></div>
          </div>
        </div>
      </form>
      {confirmDelete && (
        <Confirm title={`Delete ${name}?`} action="Delete app" busy={busy} onConfirm={remove} onClose={() => setConfirmDelete(false)}>
          New streams can no longer be published to {name}. Streams that are live now keep running until they disconnect.
        </Confirm>
      )}
    </div>
  );
}
