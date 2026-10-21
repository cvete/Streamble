import { useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { Plus, Trash2 } from 'lucide-react';
import { del, post, put, seg } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { ago } from '../lib/format';
import type { App, Source } from '../lib/types';
import { Confirm, Dialog, Empty, ErrorNotice, Field, PageHead, StateLabel, Switch } from '../components/ui';

function SourceDialog({ initial, isNew, apps, onClose, onSaved }: {
  initial: Source; isNew: boolean; apps: string[]; onClose: () => void; onSaved: () => void;
}) {
  const [app, setApp] = useState(initial.path.split('/')[0] || apps[0] || '');
  const [name, setName] = useState(initial.path.split('/')[1] ?? '');
  const [url, setUrl] = useState(initial.url);
  const [disabled, setDisabled] = useState(initial.disabled);
  const [error, setError] = useState<unknown>(null);
  const { run, busy } = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body: Source = { path: app + '/' + name, url, disabled };
    setError(null);
    try {
      if (isNew) await post('/config/sources', body);
      else await put('/config/sources/' + seg(initial.path), body);
    } catch (err) {
      setError(err);
      return;
    }
    await run(async () => {}, isNew ? `Pulling ${body.path}.` : `Source ${body.path} saved.`);
    onSaved();
    onClose();
  };
  return (
    <Dialog title={isNew ? 'New pull source' : 'Edit ' + initial.path} onClose={onClose} footer={
      <><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" form="source-form" disabled={busy}>{isNew ? 'Add source' : 'Save'}</button></>
    }>
      <form id="source-form" className="fields" onSubmit={submit}>
        <div className="fields two">
          <Field label="App">
            <select value={app} onChange={(e) => setApp(e.target.value)} disabled={!isNew} required>
              {apps.map((a) => <option key={a} value={a}>{a}</option>)}
            </select>
          </Field>
          <Field label="Stream name">
            <input type="text" value={name} onChange={(e) => setName(e.target.value)} disabled={!isNew} required placeholder="e.g. tv1" />
          </Field>
        </div>
        <Field label="Source URL" hint="rtmp://, srt:// (caller), http(s):// MPEG-TS, MP3 or AAC radio streams, udp:// multicast.">
          <input type="text" className="mono" value={url} onChange={(e) => setUrl(e.target.value)} required spellCheck={false}
            placeholder="https://radio.example.com/live.mp3" />
        </Field>
        <Switch checked={!disabled} onChange={(v) => setDisabled(!v)} label={disabled ? 'Paused' : 'Pull now and keep reconnecting'} />
        <ErrorNotice error={error} />
      </form>
    </Dialog>
  );
}

export function Sources() {
  const cfg = useApi<{ sources: Source[] }>('/config/sources');
  const apps = useApi<{ apps: App[] }>('/config/apps');
  const { snapshot } = useLive();
  const { can } = useSession();
  const { run, busy } = useAction();
  const [editing, setEditing] = useState<{ s: Source; isNew: boolean } | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const status = new Map((snapshot?.sources ?? []).map((s) => [s.path, s]));
  const appNames = (apps.data?.apps ?? []).map((a) => a.name);
  const sources = cfg.data?.sources ?? [];

  return (
    <div className="page">
      <PageHead title="Pull sources" actions={can('operator') && (
        <button className="btn primary" disabled={appNames.length === 0} onClick={() => setEditing({ s: { path: '', url: '', disabled: false }, isNew: true })}>
          <Plus size={15} /> New source
        </button>
      )}>
        Streams the server fetches by itself, from another server, an IPTV feed or an internet radio, and then serves like any published stream.
      </PageHead>
      <ErrorNotice error={cfg.error} />
      <div className="panel">
        {cfg.data && sources.length === 0 ? (
          <Empty title="No pull sources"><p>Add a source to relay a stream you do not publish yourself.</p></Empty>
        ) : (
          <table>
            <thead><tr><th>Stream</th><th>Source URL</th><th>State</th><th className="num">Reconnects</th><th className="num">Since</th><th /></tr></thead>
            <tbody>
              {sources.map((s) => {
                const st = status.get(s.path);
                return (
                  <tr key={s.path}>
                    <td>{st?.state === 'live' ? <Link className="cell-main" to={'/streams/' + s.path}>{s.path}</Link> : <span className="cell-main">{s.path}</span>}</td>
                    <td className="mono muted" style={{ maxWidth: 340, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.url}</td>
                    <td>{s.disabled ? <StateLabel state="paused" /> : st ? <StateLabel state={st.state} title={st.last_error} /> : <StateLabel state="starting" />}
                      {st?.last_error && <div className="muted" style={{ fontSize: 'var(--t-xs)', maxWidth: 300 }}>{st.last_error}</div>}</td>
                    <td className="num">{st?.reconnects ?? '—'}</td>
                    <td className="num muted">{st ? ago(st.since) : '—'}</td>
                    <td className="num nowrap">{can('operator') && (
                      <>
                        <button className="btn ghost small" disabled={busy} onClick={() => run(() => put('/config/sources/' + seg(s.path), { ...s, disabled: !s.disabled }).then(cfg.reload),
                          s.disabled ? `Resumed ${s.path}.` : `Paused ${s.path}.`)}>{s.disabled ? 'Resume' : 'Pause'}</button>
                        <button className="btn ghost small" onClick={() => setEditing({ s, isNew: false })}>Edit</button>
                        <button className="btn ghost small icon" aria-label={'Delete ' + s.path} onClick={() => setDeleting(s.path)}><Trash2 size={13} /></button>
                      </>
                    )}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
      {editing && <SourceDialog initial={editing.s} isNew={editing.isNew} apps={appNames} onClose={() => setEditing(null)} onSaved={cfg.reload} />}
      {deleting && (
        <Confirm title={`Delete ${deleting}?`} action="Delete source" busy={busy} onClose={() => setDeleting(null)} onConfirm={async () => {
          if (await run(() => del('/config/sources/' + seg(deleting)), `Source ${deleting} deleted.`)) {
            setDeleting(null);
            cfg.reload();
          }
        }}>The server stops pulling this stream now; viewers watching it are disconnected.</Confirm>
      )}
    </div>
  );
}
