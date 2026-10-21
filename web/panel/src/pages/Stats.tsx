import { useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useApi, useStored } from '../lib/hooks';
import { useLive } from '../lib/live';
import { bytes, kbps, number } from '../lib/format';
import type { StatsPoint } from '../lib/types';
import { Chart } from '../components/Chart';
import { Empty, ErrorNotice, PageHead } from '../components/ui';

const ranges = [
  { key: '1h', label: '1 hour', secs: 3600 },
  { key: '6h', label: '6 hours', secs: 6 * 3600 },
  { key: '24h', label: '24 hours', secs: 86400 },
  { key: '7d', label: '7 days', secs: 7 * 86400 },
  { key: '30d', label: '30 days', secs: 30 * 86400 },
];

interface Summary {
  peak_viewers: number;
  viewer_minutes: number;
  bytes_in: number;
  bytes_out: number;
  streams: { path: string; viewer_minutes: number; peak_viewers: number; bytes_out: number }[] | null;
}

interface Countries {
  enabled: boolean;
  countries: { country: string; viewer_minutes?: number; viewers?: number }[] | null;
}

const regionNames = (() => {
  try {
    return new Intl.DisplayNames(['en'], { type: 'region' });
  } catch {
    return null;
  }
})();

function countryName(code: string): string {
  if (!code || code === 'ZZ' || code === '--') return 'Unknown';
  try {
    return regionNames?.of(code) ?? code;
  } catch {
    return code;
  }
}

function flag(code: string): string {
  if (!/^[A-Z]{2}$/.test(code) || code === 'ZZ') return '';
  return String.fromCodePoint(...[...code].map((c) => 0x1f1e6 + c.charCodeAt(0) - 65));
}

function hours(minutes: number): string {
  if (minutes < 120) return number(Math.round(minutes)) + ' min';
  return number(Math.round(minutes / 60)) + ' h';
}

function CountryBars({ rows, value, unit }: { rows: { country: string; v: number }[]; value: (v: number) => string; unit: string }) {
  const max = Math.max(1, ...rows.map((r) => r.v));
  if (rows.length === 0) return <p className="muted" style={{ padding: '12px 16px' }}>No {unit} yet.</p>;
  return (
    <table className="bars">
      <tbody>
        {rows.slice(0, 15).map((r) => (
          <tr key={r.country}>
            <td style={{ width: '40%' }}><span aria-hidden="true" style={{ marginRight: 8 }}>{flag(r.country)}</span>{countryName(r.country)}</td>
            <td><div className="bar"><span style={{ width: (r.v / max) * 100 + '%' }} /></div></td>
            <td className="num mono" style={{ width: 90 }}>{value(r.v)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function Stats() {
  const [range, setRange] = useStored('stats.range', '24h');
  const [path, setPath] = useState('');
  const { snapshot } = useLive();
  const secs = ranges.find((r) => r.key === range)?.secs ?? 86400;
  // Round "now" to the minute so the URLs stay stable between renders.
  const to = Math.floor(Date.now() / 60_000) * 60_000;
  const from = to - secs * 1000;
  const q = `from=${from}&to=${to}`;
  const series = useApi<{ step_seconds: number; points: StatsPoint[] | null }>(`/stats/series?path=${encodeURIComponent(path)}&${q}`);
  const summary = useApi<Summary>(`/stats/summary?${q}`);
  const countries = useApi<Countries>(`/stats/countries?path=${encodeURIComponent(path)}&${q}`);
  const live = useApi<Countries>(`/stats/countries/live?path=${encodeURIComponent(path)}`);

  const points = series.data?.points ?? [];
  const times = points.map((p) => p.ts / 1000);
  const streamPaths = useMemo(() => {
    const set = new Set<string>((summary.data?.streams ?? []).map((s) => s.path));
    for (const s of snapshot?.streams ?? []) set.add(s.path);
    return [...set].sort();
  }, [summary.data, snapshot]);

  const s = summary.data;
  const geoOff = countries.data && !countries.data.enabled;

  return (
    <div className="page">
      <PageHead title="Statistics" actions={
        <div className="tabs" role="tablist" aria-label="Time range">
          {ranges.map((r) => (
            <button key={r.key} role="tab" aria-selected={range === r.key} className={range === r.key ? 'active' : ''} onClick={() => setRange(r.key)}>{r.label}</button>
          ))}
        </div>
      }>
        Viewers and traffic recorded every minute.
      </PageHead>
      <ErrorNotice error={series.error ?? summary.error} />

      <div className="readouts">
        <div className="readout"><div className="v">{s ? number(s.peak_viewers) : '–'}</div><div className="k">peak viewers</div></div>
        <div className="readout"><div className="v">{s ? hours(s.viewer_minutes) : '–'}</div><div className="k">watched</div></div>
        <div className="readout"><div className="v">{s ? bytes(s.bytes_out) : '–'}</div><div className="k">sent to viewers</div></div>
        <div className="readout"><div className="v">{s ? bytes(s.bytes_in) : '–'}</div><div className="k">received</div></div>
      </div>

      <div className="panel" style={{ marginTop: 16 }}>
        <div className="panel-head">
          <h3>Viewers</h3>
          <select value={path} onChange={(e) => setPath(e.target.value)} aria-label="Stream" style={{ width: 'auto', minWidth: 200 }}>
            <option value="">Whole server</option>
            {streamPaths.map((p) => <option key={p} value={p}>{p}</option>)}
          </select>
        </div>
        <div className="panel-body">
          {points.length > 1 ? (
            <Chart times={times} height={220} yFormat={(v) => number(v)} series={[
              { label: 'Peak', color: '#e5484d', values: points.map((p) => p.viewers_max), format: number },
              { label: 'Average', color: '#8fb6ff', values: points.map((p) => p.viewers), fill: true, format: number },
            ]} />
          ) : <p className="muted">{series.loading ? 'Loading…' : 'Not enough data for this range yet. A point is recorded every minute.'}</p>}
        </div>
      </div>

      <div className="panel" style={{ marginTop: 16 }}>
        <div className="panel-head"><h3>Bandwidth</h3></div>
        <div className="panel-body">
          {points.length > 1 ? (
            <Chart times={times} height={180} yFormat={(v) => kbps(v)} series={[
              { label: 'Sent', color: '#8fb6ff', values: points.map((p) => p.bitrate_out_kbps), fill: true, format: kbps },
              { label: 'Received', color: '#46a758', values: points.map((p) => p.bitrate_in_kbps), format: kbps },
            ]} />
          ) : <p className="muted">No data yet.</p>}
        </div>
      </div>

      <div className="grid cols-2" style={{ marginTop: 16 }}>
        <div className="panel">
          <div className="panel-head"><h3>Top streams</h3><span className="aside">by time watched</span></div>
          <div className="panel-body flush">
            {(s?.streams ?? []).length === 0 ? (
              <Empty title="No viewers in this range" />
            ) : (
              <table>
                <thead><tr><th>Stream</th><th className="num">Watched</th><th className="num">Peak</th><th className="num">Sent</th></tr></thead>
                <tbody>
                  {(s?.streams ?? []).slice(0, 20).map((r) => (
                    <tr key={r.path}>
                      <td><Link className="cell-main" to={'/streams/' + r.path}>{r.path}</Link></td>
                      <td className="num">{hours(r.viewer_minutes)}</td>
                      <td className="num">{number(r.peak_viewers)}</td>
                      <td className="num">{bytes(r.bytes_out)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
        <div className="panel">
          <div className="panel-head"><h3>Countries</h3><span className="aside">{path || 'whole server'}</span></div>
          <div className="panel-body flush">
            {geoOff ? (
              <p className="muted" style={{ padding: '12px 16px' }}>
                Country statistics are off. Download the free GeoLite2 Country database from MaxMind and set <code>geoip.database</code> in the server config.
              </p>
            ) : (
              <>
                <div className="subhead">Watching now</div>
                <CountryBars unit="viewers right now" value={number}
                  rows={(live.data?.countries ?? []).map((c) => ({ country: c.country, v: c.viewers ?? 0 }))} />
                <div className="subhead">In this range</div>
                <CountryBars unit="viewers in this range" value={hours}
                  rows={(countries.data?.countries ?? []).map((c) => ({ country: c.country, v: c.viewer_minutes ?? 0 }))} />
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
