import { useState, type FormEvent } from 'react';
import { Plus, Trash2 } from 'lucide-react';
import { del, post, put } from '../lib/api';
import { useAction, useApi } from '../lib/hooks';
import { useSession } from '../lib/session';
import { ago, dateTime, randomKey } from '../lib/format';
import type { AuditEntry, Role, User } from '../lib/types';
import { Confirm, CopyField, Dialog, Empty, ErrorNotice, Field, PageHead, StateLabel } from '../components/ui';

const roleHelp: Record<Role, string> = {
  viewer: 'Watches streams and statistics. Keys and secrets are hidden.',
  operator: 'Manages apps, keys, transcoding, restreams and pull sources, and can disconnect streams.',
  admin: 'Everything, plus users, webhooks, playback switches and the audit log.',
};

function NewUser({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [username, setUsername] = useState('');
  const [role, setRole] = useState<Role>('operator');
  const [password] = useState(() => randomKey(16));
  const [error, setError] = useState<unknown>(null);
  const [created, setCreated] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      await post('/users', { username, password, role });
      setCreated(true);
      onCreated();
    } catch (err) {
      setError(err);
    }
  };
  if (created) {
    return (
      <Dialog title={`${username} can sign in`} onClose={onClose} footer={<button className="btn primary" onClick={onClose}>Done</button>}>
        <div className="fields">
          <p className="text-2">Send these details to {username} privately. They choose their own password when they first sign in.</p>
          <CopyField label="Address" value={window.location.origin + '/panel/'} />
          <CopyField label="User name" value={username} />
          <CopyField label="Temporary password" value={password} />
        </div>
      </Dialog>
    );
  }
  return (
    <Dialog title="New user" onClose={onClose} footer={
      <><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" form="user-form">Create user</button></>
    }>
      <form id="user-form" className="fields" onSubmit={submit}>
        <Field label="User name"><input type="text" value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="off" /></Field>
        <Field label="Role" hint={roleHelp[role]}>
          <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </Field>
        <p className="muted">A temporary password is generated and shown after you create the user.</p>
        <ErrorNotice error={error} />
      </form>
    </Dialog>
  );
}

export function Users() {
  const { data, error, reload } = useApi<{ users: User[] }>('/users');
  const { user: me } = useSession();
  const { run, busy } = useAction();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<User | null>(null);
  const [reset, setReset] = useState<{ u: User; pw: string } | null>(null);
  const update = (u: User, body: Partial<User> & { password?: string }, ok: string) =>
    run(() => put('/users/' + u.id, body), ok).then((done) => { if (done) reload(); return done; });

  return (
    <div className="page">
      <PageHead title="Users" actions={<button className="btn primary" onClick={() => setCreating(true)}><Plus size={15} /> New user</button>}>
        People who can sign in to this panel. Scripts use API tokens, created under Account.
      </PageHead>
      <ErrorNotice error={error} />
      <div className="panel">
        <table>
          <thead><tr><th>User</th><th>Role</th><th>Status</th><th className="num">Last sign-in</th><th /></tr></thead>
          <tbody>
            {(data?.users ?? []).map((u) => (
              <tr key={u.id}>
                <td className="cell-main">{u.username}{u.id === me?.id && <span className="muted"> (you)</span>}</td>
                <td>
                  <select value={u.role} disabled={u.id === me?.id || busy} aria-label={'Role of ' + u.username} style={{ width: 'auto' }}
                    onChange={(e) => update(u, { role: e.target.value as Role }, `${u.username} is now ${e.target.value}.`)}>
                    <option value="viewer">Viewer</option>
                    <option value="operator">Operator</option>
                    <option value="admin">Admin</option>
                  </select>
                </td>
                <td>{u.disabled ? <StateLabel state="disabled" /> : u.must_change_password ? <span className="text-2">Waiting for first sign-in</span> : <span className="text-2">Active</span>}</td>
                <td className="num muted">{u.last_login_at ? ago(u.last_login_at) : 'never'}</td>
                <td className="num nowrap">{u.id !== me?.id && (
                  <>
                    <button className="btn ghost small" disabled={busy} onClick={() => {
                      const pw = randomKey(16);
                      update(u, { password: pw }, `Password of ${u.username} reset.`).then((ok) => ok && setReset({ u, pw }));
                    }}>Reset password</button>
                    <button className="btn ghost small" disabled={busy} onClick={() => update(u, { disabled: !u.disabled }, u.disabled ? `${u.username} enabled.` : `${u.username} disabled and signed out.`)}>
                      {u.disabled ? 'Enable' : 'Disable'}
                    </button>
                    <button className="btn ghost small icon" aria-label={'Delete ' + u.username} onClick={() => setDeleting(u)}><Trash2 size={13} /></button>
                  </>
                )}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="grid cols-3" style={{ marginTop: 16 }}>
        {(Object.keys(roleHelp) as Role[]).map((r) => (
          <div key={r} className="panel"><div className="panel-body"><strong style={{ textTransform: 'capitalize' }}>{r}</strong><p className="text-2" style={{ marginTop: 4 }}>{roleHelp[r]}</p></div></div>
        ))}
      </div>
      {creating && <NewUser onClose={() => setCreating(false)} onCreated={reload} />}
      {reset && (
        <Dialog title={`New password for ${reset.u.username}`} onClose={() => setReset(null)} footer={<button className="btn primary" onClick={() => setReset(null)}>Done</button>}>
          <div className="fields">
            <p className="text-2">{reset.u.username} was signed out and must choose a new password at the next sign-in.</p>
            <CopyField label="Temporary password" value={reset.pw} />
          </div>
        </Dialog>
      )}
      {deleting && (
        <Confirm title={`Delete ${deleting.username}?`} action="Delete user" busy={busy} onClose={() => setDeleting(null)} onConfirm={async () => {
          if (await run(() => del('/users/' + deleting.id), `${deleting.username} deleted.`)) {
            setDeleting(null);
            reload();
          }
        }}>They are signed out and their API tokens stop working.</Confirm>
      )}
    </div>
  );
}

export function Audit() {
  const [before, setBefore] = useState('');
  const [pages, setPages] = useState<AuditEntry[]>([]);
  const { data, error } = useApi<{ entries: AuditEntry[] | null }>('/audit' + (before ? '?before=' + encodeURIComponent(before) : ''));
  const entries = [...pages, ...(data?.entries ?? [])];
  const [filter, setFilter] = useState('');
  const f = filter.toLowerCase();
  const shown = f ? entries.filter((e) => [e.username, e.action, e.target, e.detail].join(' ').toLowerCase().includes(f)) : entries;

  return (
    <div className="page">
      <PageHead title="Audit log" actions={
        <input type="search" placeholder="Filter" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter" style={{ width: 220 }} />
      }>
        Every change made through the panel or the API, and every sign-in.
      </PageHead>
      <ErrorNotice error={error} />
      <div className="panel">
        {data && entries.length === 0 ? <Empty title="Nothing recorded yet" /> : (
          <table>
            <thead><tr><th>When</th><th>Who</th><th>What</th><th>Target</th><th>Detail</th></tr></thead>
            <tbody>
              {shown.map((e) => (
                <tr key={e.id}>
                  <td className="nowrap muted">{dateTime(e.at)}</td>
                  <td>{e.username || <span className="muted">system</span>}</td>
                  <td><code>{e.action}</code></td>
                  <td className="cell-main">{e.target}</td>
                  <td className="muted">{e.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {(data?.entries?.length ?? 0) >= 200 && (
        <div style={{ marginTop: 12, textAlign: 'center' }}>
          <button className="btn" onClick={() => {
            const list = data?.entries ?? [];
            setPages([...pages, ...list]);
            setBefore(list[list.length - 1].at);
          }}>Older entries</button>
        </div>
      )}
    </div>
  );
}
