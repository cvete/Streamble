// Types mirror docs/API.md.

export type Role = 'viewer' | 'operator' | 'admin';

export interface User {
  id: string;
  username: string;
  role: Role;
  disabled: boolean;
  must_change_password: boolean;
  created_at: string;
  updated_at: string;
  last_login_at?: string;
}

export interface Track {
  codec: string;
  type: 'video' | 'audio';
}

export interface Stream {
  path: string;
  app: string;
  name: string;
  protocol: string;
  remote_addr: string;
  started_at: string;
  uptime_seconds: number;
  tracks: Track[] | null;
  bytes_in: number;
  bytes_out: number;
  bitrate_in_kbps: number;
  bitrate_out_kbps: number;
  viewers: number;
  kickable: boolean;
  parent?: string;
  renditions?: string[];
  hls: { primary: string; variants: Record<string, string>; abr?: string };
  http: Record<string, string>;
}

export interface Overview {
  streams: number;
  sources: number;
  viewers: number;
  bitrate_in_kbps: number;
  bitrate_out_kbps: number;
}

export interface TranscodeStatus {
  stream: string;
  profile: string;
  state: string;
  last_error?: string;
  since: string;
  restarts: number;
  renditions: string[] | null;
}

export interface PushStatus {
  stream: string;
  name: string;
  url: string;
  state: string;
  last_error?: string;
  since: string;
  bytes_sent: number;
  reconnects: number;
}

export interface SourceStatus {
  path: string;
  url: string;
  state: string;
  last_error?: string;
  since: string;
  reconnects: number;
}

export interface Snapshot {
  streams: Stream[];
  overview: Overview;
  transcodes: TranscodeStatus[] | null;
  pushes: PushStatus[] | null;
  sources: SourceStatus[] | null;
}

export interface ServerEvent {
  type: string;
  at: string;
  data: Record<string, unknown>;
}

export interface Push {
  name: string;
  url: string;
  streams: string[] | null;
  disabled: boolean;
}

export interface App {
  name: string;
  description: string;
  publish_key: string;
  stream_keys: Record<string, string> | null;
  srt_passphrase: string;
  play_key: string;
  auth_url: string;
  transcode: string;
  push: Push[] | null;
}

export interface Rendition {
  name: string;
  height: number;
  fps: number;
  video_bitrate: string;
  audio_bitrate: string;
  audio_only: boolean;
  audio_codec: string;
}

export interface Profile {
  name: string;
  hwaccel: string;
  vaapi_device: string;
  preset: string;
  gop: string;
  renditions: Rendition[];
}

export interface Source {
  path: string;
  url: string;
  disabled: boolean;
}

export interface Webhook {
  name: string;
  url: string;
  secret: string;
  events: string[] | null;
  disabled: boolean;
}

export interface Delivery {
  id: string;
  event: string;
  at: string;
  status: number;
  attempts: number;
  error: string;
  duration_ms: number;
}

export interface Playback {
  http_ts: boolean;
  http_audio: boolean;
  rtmp: boolean;
  srt: boolean;
}

export interface ServerInfo {
  version: string;
  source: string;
  uptime_seconds: number;
  storage: string;
  listeners: {
    rtmp: { enabled: boolean; listen: string };
    srt: { enabled: boolean; listen: string; latency_ms: number };
    http: { listen: string };
  };
  hls: { variants: string[]; primary: string; segment_count: number; segment_duration: string; part_duration: string };
  playback: Playback;
  transcode: { ffmpeg: string };
  geoip: { enabled: boolean };
  stats: { retention_days: number };
  overview: Overview;
}

export interface APIToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at?: string;
}

export interface AuditEntry {
  id: string;
  at: string;
  username: string;
  action: string;
  target: string;
  detail: string;
}

export interface StatsPoint {
  ts: string; // RFC 3339, start of the bucket
  viewers: number;
  viewers_max: number;
  bitrate_in_kbps: number;
  bitrate_out_kbps: number;
  bytes_in: number;
  bytes_out: number;
}
