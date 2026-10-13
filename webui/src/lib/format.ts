export const badgeVariant = (phase?: string): 'success' | 'warning' | 'destructive' | 'muted' => {
  const p = (phase || '').toLowerCase()
  if (p.includes('run') || p.includes('ready') || p === 'bound' || p === 'deployed') return 'success'
  if (p.includes('pend') || p.includes('progress') || p.includes('creat')) return 'warning'
  if (p) return 'destructive'
  return 'muted'
}
export const fmtWhen = (ms?: string) => (ms ? new Date(Number(ms)).toLocaleString() : '')
