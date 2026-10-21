import { useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { Plus, RotateCcw, Trash2 } from 'lucide-react';
import { del, post, put, seg } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { ago } from '../lib/format';
import type { App, Profile, Rendition } from '../lib/types';
import { Confirm, Dialog, Empty, ErrorNotice, Field, PageHead, StateLabel } from '../components/ui';

const newRendition = (): Rendition => ({
  name: '480p', height: 480, fps: 0, video_bitrate: '1200k', audio_bitrate: '96k', audio_only: false, audio_codec: 'aac',
});

const blankProfile = (): Profile => ({
  name: '', hwaccel: 'none', vaapi_device: '', preset: 'veryfast', gop: '2s',
  renditions: [
    { name: '720p', height: 720, fps: 0, video_bitrate: '2500k', audio_bitrate: '128k', audio_only: false, audio_codec: 'aac' },
    { name: '480p', height: 480, fps: 0, video_bitrate: '1200k', audio_bitrate: '96k', audio_only: false, audio_codec: 'aac' },
  ],
});

function RenditionsTable({ rs, onChange }: { rs: Rendition[]; onChange: (r: Rendition[]) => void }) {
  const set = (i: number, patch: Partial<Rendition>) => onChange(rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <div className="fields">
      <div style={{ overflowX: 'auto' }}>
        <table className="edit-table">
          <thead><tr><th>Name</th><th>Kind</th><th>Height</th><th>Max fps</th><th>Video</th><th>Audio</th><th>Audio codec</th><th /></tr></thead>
          <tbody>
            {rs.map((r, i) => (
              <tr key={i}>
                <td><input type="text" value={r.name} onChange={(e) => set(i, { name: e.target.value })} aria-label="Rendition name" style={{ width: 90 }} /></td>
                <td>
                  <select value={r.audio_only ? 'audio' : 'video'} aria-label="Kind" style={{ width: 150 }} onChange={(e) => set(i, e.target.value === 'audio'
                    ? { audio_only: true, height: 0, fps: 0, video_bitrate: '' }
                    : { audio_only: false, audio_codec: 'aac' })}>
                    <option value="video">Video</option>
                    <option value="audio">Radio (audio only)</option>
                  </select>
                </td>
                <td><input type="number" min={0} value={r.height || ''} placeholder="any" disabled={r.audio_only}
                  onChange={(e) => set(i, { height: Number(e.target.value) })} aria-label="Height" style={{ width: 80 }} /></td>
                <td><input type="number" min={0} value={r.fps || ''} placeholder="any" disabled={r.audio_only}
                  onChange={(e) => set(i, { fps: Number(e.target.value) })} aria-label="Max fps" style={{ width: 70 }} /></td>
                <td><input type="text" value={r.video_bitrate} disabled={r.audio_only} placeholder="2500k"
                  onChange={(e) => set(i, { video_bitrate: e.target.value })} aria-label="Video bitrate" style={{ width: 80 }} /></td>
                <td><input type="text" value={r.audio_bitrate} placeholder="128k"
                  onChange={(e) => set(i, { audio_bitrate: e.target.value })} aria-label="Audio bitrate" style={{ width: 70 }} /></td>
                <td>
                  <select value={r.audio_codec || 'aac'} aria-label="Audio codec" style={{ width: 80 }} onChange={(e) => set(i, { audio_codec: e.target.value })}>
                    <option value="aac">AAC</option>
                    {r.audio_only && <option value="mp3">MP3</option>}
                  </select>
                </td>
                <td><button type="button" className="btn ghost icon" aria-label={'Remove ' + r.name}
                  onClick={() => onChange(rs.filter((_, j) => j !== i))}><Trash2 size={14} /></button></td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="inline">
        <button type="button" className="btn small" onClick={() => onChange([...rs, newRendition()])}><Plus size={13} /> Video rendition</button>
        <button type="button" className="btn small" onClick={() => onChange([...rs, {
          name: 'radio', height: 0, fps: 0, video_bitrate: '', audio_bitrate: '128k', audio_only: true, audio_codec: 'mp3',
        }])}><Plus size={13} /> Radio rendition</button>
      </div>
    </div>
  );
}

function ProfileDialog({ initial, isNew, onClose, onSaved }: {
  initial: Profile; isNew: boolean; onClose: () => void; onSaved: () => void;
}) {
  const [p, setP] = useState<Profile>(initial);
  const [error, setError] = useState<unknown>(null);
  const { run, busy } = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      if (isNew) await post('/config/profiles', p);
      else await put('/config/profiles/' + seg(initial.name), p);
    } catch (err) {
      setError(err);
      return;
    }
    await run(async () => {}, isNew ? `Profile ${p.name} created.` : `Profile ${p.name} saved. Live streams using it restart their transcoder.`);
    onSaved();
    onClose();
  };
  return (
    <Dialog wide title={isNew ? 'New transcoding profile' : 'Edit ' + initial.name} onClose={onClose} footer={
      <><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" form="profile-form" disabled={busy}>{isNew ? 'Create profile' : 'Save'}</button></>
    }>
      <form id="profile-form" className="fields" onSubmit={submit}>
        <div className="fields two">
          <Field label="Name">
            <input type="text" value={p.name} disabled={!isNew} required onChange={(e) => setP({ ...p, name: e.target.value })} />
          </Field>
          <Field label="Encoder" hint="Hardware encoders need a matching GPU and FFmpeg build.">
            <select value={p.hwaccel || 'none'} onChange={(e) => setP({ ...p, hwaccel: e.target.value, preset: e.target.value === 'nvenc' ? 'p4' : e.target.value === 'none' ? 'veryfast' : '' })}>
              <option value="none">CPU (libx264)</option>
              <option value="nvenc">NVIDIA NVENC</option>
              <option value="qsv">Intel Quick Sync</option>
              <option value="vaapi">VA-API (Intel/AMD)</option>
            </select>
          </Field>
          <Field label="Speed preset" hint="Faster presets use less CPU at slightly lower quality.">
            <input type="text" value={p.preset} onChange={(e) => setP({ ...p, preset: e.target.value })} />
          </Field>
          <Field label="Keyframe interval" hint="Matches the HLS segment length, e.g. 2s.">
            <input type="text" value={p.gop} onChange={(e) => setP({ ...p, gop: e.target.value })} />
          </Field>
          {p.hwaccel === 'vaapi' && (
            <Field label="VA-API device"><input type="text" className="mono" value={p.vaapi_device} placeholder="/dev/dri/renderD128" onChange={(e) => setP({ ...p, vaapi_device: e.target.value })} /></Field>
          )}
        </div>
        <Field label="Renditions" hint="Each rendition is published as <stream>_<name>, e.g. tv1_720p, and joins the adaptive HLS playlist.">
          <RenditionsTable rs={p.renditions} onChange={(r) => setP({ ...p, renditions: r })} />
        </Field>
        <ErrorNotice error={error} />
      </form>
    </Dialog>
  );
}

export function Transcoding() {
  const profiles = useApi<{ profiles: Profile[] }>('/config/profiles');
  const apps = useApi<{ apps: App[] }>('/config/apps');
  const { snapshot } = useLive();
  const { can } = useSession();
  const { run, busy } = useAction();
  const [editing, setEditing] = useState<{ p: Profile; isNew: boolean } | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const jobs = snapshot?.transcodes ?? [];
  const usedBy = (name: string) => (apps.data?.apps ?? []).filter((a) => a.transcode === name).map((a) => a.name);

  return (
    <div className="page">
      <PageHead title="Transcoding" actions={can('operator') && (
        <button className="btn primary" onClick={() => setEditing({ p: blankProfile(), isNew: true })}><Plus size={15} /> New profile</button>
      )}>
        Profiles turn one incoming stream into several qualities for adaptive playback, or into radio-friendly audio. Assign a profile to an app in Apps and keys.
      </PageHead>

      <div className="panel">
        <div className="panel-head"><h3>Running now</h3><span className="aside">{jobs.length} {jobs.length === 1 ? 'transcoder' : 'transcoders'}</span></div>
        <div className="panel-body flush">
          {jobs.length === 0 ? (
            <p className="muted" style={{ padding: '12px 16px' }}>No stream is being transcoded. A transcoder starts when a stream goes on air in an app with a profile.</p>
          ) : (
            <table>
              <thead><tr><th>Stream</th><th>Profile</th><th>State</th><th>Renditions</th><th className="num">Restarts</th><th className="num">Since</th><th /></tr></thead>
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.stream}>
                    <td><Link className="cell-main" to={'/streams/' + j.stream}>{j.stream}</Link></td>
                    <td className="text-2">{j.profile}</td>
                    <td><StateLabel state={j.state} title={j.last_error} />
                      {j.last_error && <div className="muted" style={{ fontSize: 'var(--t-xs)' }}>{j.last_error}</div>}</td>
                    <td className="text-2">{(j.renditions ?? []).map((r) => r.split('/').pop()).join(', ') || '—'}</td>
                    <td className="num">{j.restarts}</td>
                    <td className="num muted">{ago(j.since)}</td>
                    <td className="num">{can('operator') && j.renditions?.length ? (
                      <button className="btn ghost small" disabled={busy} onClick={() => run(() => post('/streams/' + seg(j.renditions![0]) + '/kick'), 'Transcoder restarting.')}>
                        <RotateCcw size={13} /> Restart
                      </button>
                    ) : null}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <h2 className="section-title">Profiles</h2>
      <ErrorNotice error={profiles.error} />
      {profiles.data && profiles.data.profiles.length === 0 ? (
        <div className="panel"><Empty title="No transcoding profiles"><p>Streams are delivered exactly as the encoder sends them.</p></Empty></div>
      ) : (
        <div className="grid cols-2">
          {(profiles.data?.profiles ?? []).map((p) => (
            <div className="panel" key={p.name}>
              <div className="panel-head">
                <h3>{p.name}</h3>
                <span className="aside">{[p.hwaccel === 'none' || !p.hwaccel ? 'CPU' : p.hwaccel.toUpperCase(), p.preset, p.gop && p.gop !== '0s' ? 'keyframe every ' + p.gop : ''].filter(Boolean).join(' · ')}</span>
              </div>
              <div className="panel-body flush">
                <table>
                  <tbody>
                    {p.renditions.map((r) => (
                      <tr key={r.name}>
                        <td className="cell-main">{r.name}</td>
                        <td className="text-2">{r.audio_only ? `Radio, ${(r.audio_codec || 'aac').toUpperCase()}` : `${r.height ? r.height + 'p' : 'source size'}${r.fps ? ' at ' + r.fps + ' fps' : ''}`}</td>
                        <td className="num mono">{r.audio_only ? '' : r.video_bitrate}</td>
                        <td className="num mono">{r.audio_bitrate}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="panel-foot">
                <span className="muted">{usedBy(p.name).length ? 'Used by ' + usedBy(p.name).join(', ') : 'Not used by any app'}</span>
                {can('operator') && (
                  <span className="inline">
                    <button className="btn ghost small" onClick={() => setDeleting(p.name)} disabled={usedBy(p.name).length > 0}
                      title={usedBy(p.name).length ? 'Remove it from the apps first' : undefined}><Trash2 size={13} /> Delete</button>
                    <button className="btn small" onClick={() => setEditing({ p, isNew: false })}>Edit</button>
                  </span>
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      {editing && <ProfileDialog initial={editing.p} isNew={editing.isNew} onClose={() => setEditing(null)} onSaved={profiles.reload} />}
      {deleting && (
        <Confirm title={`Delete ${deleting}?`} action="Delete profile" busy={busy} onClose={() => setDeleting(null)} onConfirm={async () => {
          if (await run(() => del('/config/profiles/' + seg(deleting)), `Profile ${deleting} deleted.`)) {
            setDeleting(null);
            profiles.reload();
          }
        }}>The profile is removed. No app uses it.</Confirm>
      )}
    </div>
  );
}
