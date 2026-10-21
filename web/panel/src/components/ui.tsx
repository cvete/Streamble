import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { Check, Copy, Eye, EyeOff, RefreshCw, X } from 'lucide-react';
import { ApiError } from '../lib/api';
import { randomKey } from '../lib/format';

const stateTone: Record<string, string> = {
  live: 'ok', running: 'ok',
  connecting: 'warn', starting: 'warn', retrying: 'warn', restarting: 'warn',
  failed: 'bad',
};

export function Tally({ tone }: { tone?: 'on' | 'ok' | 'warn' | 'bad' | 'off' }) {
  return <span className={'tally ' + (tone ?? '')} aria-hidden="true" />;
}

/** A status word with its tally lamp, e.g. "live", "retrying". */
export function StateLabel({ state, title }: { state: string; title?: string }) {
  const tone = (stateTone[state] ?? 'off') as 'ok' | 'warn' | 'bad' | 'off';
  return (
    <span className={'state ' + state} title={title}>
      <Tally tone={tone} />
      {state}
    </span>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint && <small className="hint">{hint}</small>}
    </label>
  );
}

export function Switch({ checked, onChange, label, disabled }: {
  checked: boolean; onChange: (v: boolean) => void; label: ReactNode; disabled?: boolean;
}) {
  return (
    <label className="switch">
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span className="track" aria-hidden="true" />
      <span>{label}</span>
    </label>
  );
}

export function CopyField({ value, label }: { value: string; label?: string }) {
  const [done, setDone] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Clipboard API needs HTTPS; fall back to a hidden textarea.
      const ta = document.createElement('textarea');
      ta.value = value;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    setDone(true);
    window.setTimeout(() => setDone(false), 1500);
  };
  return (
    <div>
      {label && <div className="lbl">{label}</div>}
      <div className="copy">
        <code title={value}>{value}</code>
        <button type="button" className="btn small icon" onClick={copy} aria-label={'Copy ' + (label ?? 'value')}>
          {done ? <Check size={14} /> : <Copy size={14} />}
        </button>
      </div>
    </div>
  );
}

/** A secret input with show/hide and a generator. */
export function SecretField({ value, onChange, label, hint, generate = true, minLength }: {
  value: string; onChange: (v: string) => void; label: string; hint?: ReactNode; generate?: boolean; minLength?: number;
}) {
  const [shown, setShown] = useState(false);
  const id = useId();
  const masked = value === '********';
  return (
    <div className="field">
      <label htmlFor={id}><span className="text-2" style={{ fontSize: 'var(--t-s)' }}>{label}</span></label>
      <div className="inline">
        <input id={id} type={shown ? 'text' : 'password'} className="mono" value={value} autoComplete="off"
          minLength={minLength} onChange={(e) => onChange(e.target.value)} placeholder="Not set" />
        <button type="button" className="btn icon" onClick={() => setShown(!shown)} aria-label={shown ? 'Hide' : 'Show'} disabled={masked}>
          {shown ? <EyeOff size={15} /> : <Eye size={15} />}
        </button>
        {generate && (
          <button type="button" className="btn icon" onClick={() => { onChange(randomKey()); setShown(true); }} aria-label="Generate a new key" title="Generate a new key">
            <RefreshCw size={15} />
          </button>
        )}
      </div>
      {hint && <small className="hint">{hint}</small>}
    </div>
  );
}

export function ErrorNotice({ error }: { error: unknown }) {
  if (!error) return null;
  const e = error instanceof ApiError ? error : new ApiError(0, String(error));
  return (
    <div className="notice error" role="alert">
      {e.message}
      {e.details.length > 0 && <ul>{e.details.map((d) => <li key={d}>{d}</li>)}</ul>}
    </div>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
    </div>
  );
}

/** Modal dialog: Escape closes, focus moves inside on open and back after. */
export function Dialog({ title, onClose, children, footer, wide }: {
  title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    const first = ref.current?.querySelector<HTMLElement>('input, select, textarea, button:not(.dialog-close)');
    first?.focus();
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
      prev?.focus();
    };
  }, [onClose]);
  return (
    <div className="scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={'dialog' + (wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={title} ref={ref}>
        <div className="dialog-head">
          <h2>{title}</h2>
          <button type="button" className="btn ghost icon dialog-close" onClick={onClose} aria-label="Close"><X size={16} /></button>
        </div>
        <div className="dialog-body">{children}</div>
        {footer && <div className="dialog-foot">{footer}</div>}
      </div>
    </div>
  );
}

export function Confirm({ title, children, action, danger = true, onConfirm, onClose, busy }: {
  title: string; children: ReactNode; action: string; danger?: boolean;
  onConfirm: () => void; onClose: () => void; busy?: boolean;
}) {
  return (
    <Dialog title={title} onClose={onClose} footer={
      <>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button type="button" className={'btn ' + (danger ? 'danger solid' : 'primary')} onClick={onConfirm} disabled={busy}>{action}</button>
      </>
    }>
      <p className="text-2">{children}</p>
    </Dialog>
  );
}

/** Small LED bar meter for a recent series, scaled to its own max. */
export function Meter({ values, bars = 14 }: { values: number[]; bars?: number }) {
  const last = values.length ? values[values.length - 1] : 0;
  const max = Math.max(...values, 1);
  const lit = Math.round((last / max) * bars);
  return (
    <span className="meter" aria-hidden="true">
      {Array.from({ length: bars }, (_, i) => (
        <i key={i} className={i < lit ? (i >= bars - 2 ? 'hot' : 'lit') : ''} style={{ height: 4 + i * (10 / bars) }} />
      ))}
    </span>
  );
}

export function PageHead({ title, children, actions }: { title: string; children?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {children && <p>{children}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  );
}
