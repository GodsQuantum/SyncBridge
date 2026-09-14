export type Locale = 'en' | 'fr';

export function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const h = Math.floor(total / 3600), m = Math.floor((total % 3600) / 60), s = total % 60;
  if (h) return `${h}h${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

export function cronHuman(value: string, locale: Locale): string {
  const parts = value.trim().split(/\s+/);
  if (parts.length !== 5) return value;
  const [minute, hour, day, month, weekday] = parts;
  if (month !== '*') return value;
  if (minute.startsWith('*/') && hour === '*' && day === '*' && weekday === '*')
    return locale === 'fr' ? `toutes les ${minute.slice(2)} min` : `every ${minute.slice(2)} min`;
  if (minute === '0' && hour === '*' && day === '*' && weekday === '*')
    return locale === 'fr' ? 'toutes les heures' : 'every hour';
  const fixed = /^\d+$/.test(hour) && /^\d+$/.test(minute)
    ? `${String(Number(hour)).padStart(2, '0')}:${String(Number(minute)).padStart(2, '0')}` : '';
  if (!fixed) return value;
  if (day === '*' && weekday === '1-5') return locale === 'fr' ? `du lundi au vendredi à ${fixed}` : `Monday–Friday at ${fixed}`;
  if (day === '*' && (weekday === '0' || weekday === '7')) return locale === 'fr' ? `le dimanche à ${fixed}` : `Sunday at ${fixed}`;
  if (day === '*' && weekday === '*') return locale === 'fr' ? `chaque jour à ${fixed}` : `every day at ${fixed}`;
  if (/^\d+$/.test(day) && weekday === '*') return locale === 'fr' ? `le ${day} du mois à ${fixed}` : `day ${day} of the month at ${fixed}`;
  return value;
}
