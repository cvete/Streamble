import { useState, type FormEvent } from 'react';
import { Plus, Send, Trash2 } from 'lucide-react';
import { del, post, put, seg } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { dateTime } from '../lib/format';
import type { Delivery, Webhook } from '../lib/types';
import { Confirm, Dialog, Empty, ErrorNotice, Field, PageHead, SecretField, StateLabel, Switch, Tally } from '../components/ui';

const eventHelp: Record<string, string> = {
  'stream.published': 'A stream went on air',
  'stream.ended': 'A stream went off air',
  'push.state': 'A restream connected, failed or is retrying',
  'source.state': 'A pull source connected or lost its signal',
  'transcode.state': 'A transcoder started, failed or restarted',
};

function WebhookDialog({ initial, isNew, allEvents, onClose, onSaved }: {
  initial: Webhook; isNew: boolean; allEvents: string[]; onClose: () => void; onSaved: () => void;
}) {
  const [w, setW] = useState<Webhook>(initial);
  const [error, setError] = useState<unknown>(null);
  const { run, busy } = useAction();
  const events = w.events ?? [];
  const toggle = (e: string, on: boolean) => setW({ ...w, events: on ? [...events, e] : events.filter((x) => x !== e) });
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      if (isNew) await post('/config/webhooks', w);
      else await put('/config/webhooks/' + seg(initial.name), w);
    } catch (err) {
      setError(err);
      return;
    }
    await run(async () => {}, `Webhook ${w.name} saved.`);
    onSaved();
    onClose();
  };
  return (
    <Dialog title={isNew ? 'New webhook' : 'Edit ' + initial.name} onClose={onClose} footer={
      <><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" form="hook-form" disabled={busy}>{isNew ? 'Create webhook' : 'Save'}</button></>
    }>
      <form id="hook-form" className="fields" onSubmit={submit}>
        <Field label="Name"><input type="text" value={w.name} disabled={!isNew} required onChange={(e) => setW({ ...w, name: e.target.value })} /></Field>
        <Field label="URL" hint="Receives a JSON POST for every selected event.">
          <input type="url" className="mono" value={w.url} required placeholder="https://example.com/hooks/streamble" onChange={(e) => setW({ ...w, url: e.target.value })} />
        </Field>
        <SecretField label="Signing secret" value={w.secret} onChange={(v) => setW({ ...w, secret: v })}
          hint="Each request carries X-Streamble-Signature: sha256=HMAC of the body with this secret." />
        <Field label="Events" hint="None selected means every event.">
          <div className="fields" style={{ gap: 6 }}>
            {allEvents.map((e) => (
              <label key={e} className="check">
                <input type="checkbox" checked={events.includes(e)} onChange={(ev) => toggle(e, ev.target.checked)} />
                <code>{e}</code> <span className="muted">{eventHelp[e] ?? ''}</span>
              </label>
            ))}
          </div>
        </Field>
        <Switch checked={!w.disabled} onChange={(v) => setW({ ...w, disabled: !v })} label={w.disabled ? 'Paused' : 'Active'} />
        <ErrorNotice error={error} />
      </form>
    </Dialog>
  );
}

function Deliveries({ name }: { name: string }) {
  const { data, error, reload } = useApi<{ deliveries: Delivery[] | null }>('/config/webhooks/' + seg(name) + '/deliveries');
  const { run, busy } = useAction();
  const list = data?.deliveries ?? [];
  return (
    <>
      <div className="panel-foot">
        <span className="muted">Recent deliveries</span>
        <button className="btn small" disabled={busy} onClick={async () => {
          await run(() => post('/config/webhooks/' + seg(name) + '/test'), 'Test event sent.');
          reload();
        }}><Send size={13} /> Send test</button>
      </div>
      <ErrorNotice error={error} />
      {list.length === 0 ? (
        <p className="muted" style={{ padding: '0 16px 12px' }}>Nothing sent yet.</p>
      ) : (
        <table>
          <tbody>
            {list.slice(0, 8).map((d) => (
              <tr key={d.id}>
                <td className="nowrap muted">{dateTime(d.at)}</td>
                <td><code>{d.event}</code></td>
                <td>{d.status >= 200 && d.status < 300
                  ? <span className="state"><Tally tone="ok" />Delivered</span>
                  : <span className="state" style={{ color: 'var(--bad)' }}><Tally tone="bad" />Failed</span>}
                  <span className="muted" style={{ marginLeft: 6 }}>{d.status ? 'HTTP ' + d.status : d.error}</span></td>
                <td className="num muted">{d.attempts > 1 ? d.attempts + ' tries · ' : ''}{d.duration_ms} ms</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

export function Webhooks() {
  const { data, error, reload } = useApi<{ webhooks: Webhook[]; events: string[] }>('/config/webhooks');
  const { run, busy } = useAction();
  const [editing, setEditing] = useState<{ w: Webhook; isNew: boolean } | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const hooks = data?.webhooks ?? [];
  const allEvents = data?.events ?? Object.keys(eventHelp);

  return (
    <div className="page">
      <PageHead title="Webhooks" actions={
        <button className="btn primary" onClick={() => setEditing({ w: { name: '', url: '', secret: '', events: [], disabled: false }, isNew: true })}>
          <Plus size={15} /> New webhook
        </button>
      }>
        Tell other systems when streams start and stop or a restream fails: chat bots, schedulers, monitoring. To approve or refuse publishers and viewers, set an auth callback URL on the app instead.
      </PageHead>
      <ErrorNotice error={error} />
      {data && hooks.length === 0 ? (
        <div className="panel"><Empty title="No webhooks"><p>Add one to get a POST request on every stream event.</p></Empty></div>
      ) : (
        <div className="grid cols-2">
          {hooks.map((w) => (
            <div className="panel" key={w.name}>
              <div className="panel-head">
                <h3>{w.name}</h3>
                <span className="inline">
                  {w.disabled && <StateLabel state="paused" />}
                  <button className="btn ghost small" onClick={() => setEditing({ w, isNew: false })}>Edit</button>
                  <button className="btn ghost small icon" aria-label={'Delete ' + w.name} onClick={() => setDeleting(w.name)}><Trash2 size={13} /></button>
                </span>
              </div>
              <div className="panel-body">
                <div className="mono text-2" style={{ wordBreak: 'break-all' }}>{w.url}</div>
                <div className="muted" style={{ marginTop: 6 }}>{w.events?.length ? w.events.join(', ') : 'Every event'}</div>
              </div>
              <Deliveries name={w.name} />
            </div>
          ))}
        </div>
      )}
      {editing && <WebhookDialog initial={editing.w} isNew={editing.isNew} allEvents={allEvents} onClose={() => setEditing(null)} onSaved={reload} />}
      {deleting && (
        <Confirm title={`Delete ${deleting}?`} action="Delete webhook" busy={busy} onClose={() => setDeleting(null)} onConfirm={async () => {
          if (await run(() => del('/config/webhooks/' + seg(deleting)), `Webhook ${deleting} deleted.`)) {
            setDeleting(null);
            reload();
          }
        }}>Events stop being sent to this URL.</Confirm>
      )}
    </div>
  );
}
