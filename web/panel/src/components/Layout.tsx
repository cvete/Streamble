import { useState, type ReactNode } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import {
  BarChart3, Boxes, Cable, History, KeyRound, LogOut, Menu, MonitorPlay, Radio,
  Send, Settings, SlidersHorizontal, Users, Webhook,
} from 'lucide-react';
import { useLive } from '../lib/live';
import { useSession } from '../lib/session';
import { useTick } from '../lib/hooks';
import { clock, kbps, number } from '../lib/format';
import { Tally } from './ui';

function Clock() {
  const now = new Date(useTick(1000));
  return (
    <span className="station-clock" aria-label="Server time">
      <span className="date">{now.toLocaleDateString('en-GB', { weekday: 'short', day: 'numeric', month: 'short' })}</span>
      {clock(now)}
    </span>
  );
}

export function Layout({ children }: { children: ReactNode }) {
  const { snapshot, connected } = useLive();
  const { user, logout, can } = useSession();
  const [menuOpen, setMenuOpen] = useState(false);
  const location = useLocation();
  const o = snapshot?.overview;

  const failing = (snapshot?.pushes ?? []).filter((p) => p.state !== 'live').length;
  const sourceProblems = (snapshot?.sources ?? []).filter((s) => s.state !== 'live').length;

  const link = (to: string, icon: ReactNode, label: string, extra?: ReactNode) => (
    <NavLink to={to} end={to === '/'} onClick={() => setMenuOpen(false)}>
      {icon}
      {label}
      {extra}
    </NavLink>
  );

  return (
    <div className="shell">
      <nav className={'rail' + (menuOpen ? ' open' : '')} aria-label="Main">
        <div className="brand"><span className="brand-mark" aria-hidden="true" />Streamble</div>
        {link('/', <MonitorPlay size={17} />, 'Overview')}
        {link('/streams', <Radio size={17} />, 'Streams',
          o && o.streams > 0 ? <span className="count onair">{o.streams}</span> : null)}
        {link('/stats', <BarChart3 size={17} />, 'Statistics')}

        <div className="rail-group">Delivery</div>
        {link('/apps', <Boxes size={17} />, 'Apps and keys')}
        {link('/transcoding', <SlidersHorizontal size={17} />, 'Transcoding')}
        {link('/restreams', <Send size={17} />, 'Restreams',
          failing > 0 ? <span className="count" style={{ color: 'var(--warn)' }}>{failing}</span> : null)}
        {link('/sources', <Cable size={17} />, 'Pull sources',
          sourceProblems > 0 ? <span className="count" style={{ color: 'var(--warn)' }}>{sourceProblems}</span> : null)}

        <div className="rail-group">Server</div>
        {can('admin') && link('/webhooks', <Webhook size={17} />, 'Webhooks')}
        {can('admin') && link('/users', <Users size={17} />, 'Users')}
        {can('admin') && link('/audit', <History size={17} />, 'Audit log')}
        {link('/settings', <Settings size={17} />, 'Settings')}
        {link('/account', <KeyRound size={17} />, 'Account')}

        <div className="rail-foot">
          Signed in as <strong style={{ color: 'var(--text-2)' }}>{user?.username}</strong> ({user?.role})
          <br />
          <button type="button" className="btn ghost small" style={{ padding: 0, marginTop: 6 }} onClick={logout}>
            <LogOut size={13} /> Sign out
          </button>
        </div>
      </nav>

      <header className="topbar">
        <button type="button" className="btn ghost icon mobile-only" onClick={() => setMenuOpen(!menuOpen)} aria-label="Menu" aria-expanded={menuOpen}>
          <Menu size={18} />
        </button>
        <div className="status-strip">
          <span className="strip-item" title="Streams on air">
            <Tally tone={o && o.streams > 0 ? 'on' : 'off'} />
            <span className="v">{o ? o.sources : '–'}</span>
            <span className="k">on air{o && o.streams > o.sources ? ` + ${o.streams - o.sources} renditions` : ''}</span>
          </span>
          <span className="strip-item">
            <span className="v">{o ? number(o.viewers) : '–'}</span><span className="k">viewers</span>
          </span>
          <span className="strip-item hide-s">
            <span className="v">{o ? kbps(o.bitrate_in_kbps) : '–'}</span><span className="k">in</span>
          </span>
          <span className="strip-item hide-s">
            <span className="v">{o ? kbps(o.bitrate_out_kbps) : '–'}</span><span className="k">out</span>
          </span>
        </div>
        <span className={'conn hide-s' + (connected ? '' : ' lost')} title={connected ? 'Live updates connected' : 'Reconnecting to the server'}>
          <span className="dot" />{connected ? 'Live' : 'Reconnecting'}
        </span>
        <Clock />
      </header>

      <main className="main" key={location.pathname.split('/')[1]}>{children}</main>
    </div>
  );
}
