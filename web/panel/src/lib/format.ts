export function kbps(v: number): string {
  if (!v) return '0 kbps';
  if (v >= 1_000_000) return (v / 1_000_000).toFixed(2) + ' Gbps';
  if (v >= 1000) return (v / 1000).toFixed(v >= 10_000 ? 0 : 1) + ' Mbps';
  return Math.round(v) + ' kbps';
}

export function bytes(n: number): string {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  let v = n;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
}

/** Uptime as broadcast timecode: H:MM:SS, with days when long. */
export function duration(secs: number): string {
  secs = Math.max(0, Math.floor(secs));
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  const hms = `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
  return d > 0 ? `${d}d ${hms}` : hms;
}

export function number(n: number): string {
  return new Intl.NumberFormat('en').format(Math.round(n));
}

export function ago(iso: string): string {
  if (!iso || iso.startsWith('0001')) return 'never';
  const secs = (Date.now() - new Date(iso).getTime()) / 1000;
  if (secs < 45) return 'just now';
  if (secs < 3600) return Math.round(secs / 60) + ' min ago';
  if (secs < 86400) return Math.round(secs / 3600) + ' h ago';
  return Math.round(secs / 86400) + ' days ago';
}

export function dateTime(iso: string): string {
  if (!iso || iso.startsWith('0001')) return '—';
  return new Date(iso).toLocaleString('en-GB', { dateStyle: 'medium', timeStyle: 'short' });
}

export function clock(d: Date): string {
  return d.toLocaleTimeString('en-GB', { hour12: false });
}

export function codecs(tracks: { codec: string }[] | null): string {
  return (tracks ?? []).map((t) => t.codec).join(' + ') || 'no tracks';
}

/** Random key for publish/play keys: 24 URL-safe characters. */
export function randomKey(len = 24): string {
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
  const buf = new Uint32Array(len);
  crypto.getRandomValues(buf);
  return Array.from(buf, (x) => alphabet[x % alphabet.length]).join('');
}

/** Host part for building ingest URLs shown to operators. */
export function serverHost(): string {
  return window.location.hostname;
}

export function portOf(listen: string): string {
  const i = listen.lastIndexOf(':');
  return i >= 0 ? listen.slice(i + 1) : listen;
}
