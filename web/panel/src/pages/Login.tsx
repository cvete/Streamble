import { useState, type FormEvent } from 'react';
import { ApiError, post } from '../lib/api';
import { useSession } from '../lib/session';
import type { User } from '../lib/types';
import { ErrorNotice, Field } from '../components/ui';

export function Login() {
  const { login } = useSession();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(username, password);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <div className="login-card">
        <div className="brand"><span className="brand-mark" aria-hidden="true" />Streamble</div>
        <form className="panel" onSubmit={submit}>
          <div className="panel-body fields">
            <h2>Sign in</h2>
            <Field label="User name">
              <input type="text" autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus />
            </Field>
            <Field label="Password">
              <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
            </Field>
            <ErrorNotice error={error} />
            <button className="btn primary" type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
            <p className="muted" style={{ fontSize: 'var(--t-xs)' }}>
              First start? The password for <code>admin</code> is printed in the server log, or set with STREAMBLE_ADMIN_PASSWORD.
            </p>
          </div>
        </form>
      </div>
    </div>
  );
}

export function ChangePassword({ forced }: { forced?: boolean }) {
  const { setUser, logout } = useSession();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [repeat, setRepeat] = useState('');
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (next !== repeat) {
      setError(new ApiError(400, 'The new passwords do not match.'));
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await post<{ user: User }>('/auth/password', { current_password: current, new_password: next });
      setUser(res.user);
      setCurrent('');
      setNext('');
      setRepeat('');
      setDone(true);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  const form = (
    <form className="fields" onSubmit={submit}>
      <Field label="Current password">
        <input type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} required />
      </Field>
      <Field label="New password" hint="At least 10 characters. Other browsers signed in as you will be signed out.">
        <input type="password" autoComplete="new-password" minLength={10} value={next} onChange={(e) => setNext(e.target.value)} required />
      </Field>
      <Field label="Repeat the new password">
        <input type="password" autoComplete="new-password" minLength={10} value={repeat} onChange={(e) => setRepeat(e.target.value)} required />
      </Field>
      <ErrorNotice error={error} />
      {done && !forced && <div className="notice">Password changed.</div>}
      <div className="inline">
        <button className="btn primary" type="submit" disabled={busy}>Change password</button>
        {forced && <button className="btn ghost" type="button" onClick={logout}>Sign out</button>}
      </div>
    </form>
  );

  if (!forced) return form;
  return (
    <div className="login">
      <div className="login-card">
        <div className="brand"><span className="brand-mark" aria-hidden="true" />Streamble</div>
        <div className="panel">
          <div className="panel-body fields">
            <h2>Choose your own password</h2>
            <p className="text-2">Your password was set by someone else. Choose a new one to continue.</p>
            {form}
          </div>
        </div>
      </div>
    </div>
  );
}
