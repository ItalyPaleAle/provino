// Formatting helpers shared by the components

export function formatBytes(n?: number): string {
  if (!n) {
    return "–";
  }
  return n < 1024 * 1024 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1024 / 1024).toFixed(2)} MB`;
}

export function formatTime(iso: string): string {
  return new Date(iso).toLocaleTimeString();
}

export function formatBox(r: { width: number; height: number }): string {
  return `${r.width || "auto"}×${r.height || "auto"}`;
}

// Shorten an ID for a list, keeping its start, which is what tells UUIDs apart
export function shortID(id: string, length = 8): string {
  return id.length > length ? `${id.slice(0, length)}…` : id;
}
