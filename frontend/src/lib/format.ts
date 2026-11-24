const UNITS = ["B", "KiB", "MiB", "GiB", "TiB"];

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "-";
  let v = n;
  let u = 0;
  while (v >= 1024 && u < UNITS.length - 1) {
    v /= 1024;
    u++;
  }
  return `${u === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${UNITS[u]}`;
}

const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return "-";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "-" : dateTime.format(d);
}

const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

export function formatRelative(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return "never";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "-";
  const diff = (t - now) / 1000;
  const abs = Math.abs(diff);
  if (abs < 45) return relative.format(Math.round(diff), "second");
  if (abs < 2700) return relative.format(Math.round(diff / 60), "minute");
  if (abs < 64800) return relative.format(Math.round(diff / 3600), "hour");
  if (abs < 2592000) return relative.format(Math.round(diff / 86400), "day");
  if (abs < 31536000) return relative.format(Math.round(diff / 2592000), "month");
  return relative.format(Math.round(diff / 31536000), "year");
}

export function shortCommit(hash: string): string {
  return hash ? hash.slice(0, 12) : "-";
}

export function shortImageId(id: string): string {
  return id.replace(/^sha256:/, "").slice(0, 12);
}

export function formatPercent(n: number): string {
  return Number.isFinite(n) ? `${n.toFixed(1)}%` : "-";
}

/** A duration in seconds as "3d 4h", "2h 5m", "4m 10s" or "12s". */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "-";
  const s = Math.floor(seconds);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}
