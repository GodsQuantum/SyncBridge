import { describe, expect, it } from 'vitest';
import { cronHuman, formatDuration } from './format';

describe('format helpers', () => {
  it('formats common cron schedules without locale-dependent parsing', () => {
    expect(cronHuman('*/15 * * * *', 'en')).toBe('every 15 min');
    expect(cronHuman('0 3 * * 1-5', 'fr')).toBe('du lundi au vendredi à 03:00');
  });

  it('formats compact durations', () => {
    expect(formatDuration(5)).toBe('5s');
    expect(formatDuration(65)).toBe('1m05s');
    expect(formatDuration(3661)).toBe('1h01m');
  });
});
