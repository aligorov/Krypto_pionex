import { useEffect, useState } from 'react';
import { api } from '../api';

interface FngResponse {
  data: { value: number; classification: string };
}

interface EventsResponse {
  data: { title: string; eventTime: string; impact: string }[];
}

interface LiquidationsResponse {
  data: { total1hUSD: number; cascade: boolean };
}

interface SmartData {
  fng: { value: number; classification: string } | null;
  events: { title: string; eventTime: string; impact: string }[];
  liquidations: { total1hUSD: number; cascade: boolean } | null;
}

const REFRESH_MS = 60_000;

/**
 * SmartDataBadge — compact market-intelligence strip (Fear & Greed,
 * high-impact economic events, liquidation cascades) for the Overview
 * header. Silently hides itself while no endpoint has answered yet.
 */
export function SmartDataBadge() {
  const [data, setData] = useState<SmartData | null>(null);

  useEffect(() => {
    const load = async () => {
      const [fng, events, liq] = await Promise.allSettled([
        api<FngResponse>('/api/market/fng'),
        api<EventsResponse>('/api/market/events?hours=24'),
        api<LiquidationsResponse>('/api/market/liquidations'),
      ]);
      const next: SmartData = {
        fng: fng.status === 'fulfilled' ? fng.value.data : null,
        events: events.status === 'fulfilled' ? events.value.data ?? [] : [],
        liquidations: liq.status === 'fulfilled' ? liq.value.data : null,
      };
      setData(next);
    };
    void load();
    const timer = window.setInterval(() => void load(), REFRESH_MS);
    return () => window.clearInterval(timer);
  }, []);

  if (!data) return null;
  if (!data.fng && data.events.length === 0 && !data.liquidations) return null;

  const fngValue = data.fng ? Math.round(data.fng.value) : null;
  // Extreme Greed (>85) is a contrarian red flag, Extreme Fear (<15) a green one.
  const fngColor =
    fngValue === null ? 'var(--muted)' : fngValue > 85 ? 'var(--danger)' : fngValue < 15 ? 'var(--accent)' : 'var(--muted)';
  const nextEvent = data.events[0];

  return (
    <div
      style={{
        display: 'flex',
        gap: '0.5rem',
        alignItems: 'center',
        flexWrap: 'wrap',
        fontSize: '0.8rem',
      }}
    >
      {data.fng && fngValue !== null && (
        <span
          className="badge"
          title={`Fear & Greed Index: ${fngValue} (${data.fng.classification}) — контрарианский индикатор сентимента`}
          style={{ background: 'var(--surface-soft)', color: fngColor, border: `1px solid ${fngColor}` }}
        >
          🧠 FNG: {fngValue} ({data.fng.classification})
        </span>
      )}

      {nextEvent && (
        <span
          className="badge"
          title={data.events
            .map((event) => `${new Date(event.eventTime).toLocaleString('ru-RU')} · ${event.impact} · ${event.title}`)
            .join('\n')}
          style={{ background: 'rgba(245, 158, 11, 0.15)', color: 'var(--warning)', border: '1px solid rgba(245, 158, 11, 0.3)' }}
        >
          ⚠️ {nextEvent.title} в{' '}
          {new Date(nextEvent.eventTime).toLocaleString('ru-RU', { hour: '2-digit', minute: '2-digit' })}
          {data.events.length > 1 && ` (+${data.events.length - 1})`}
        </span>
      )}

      {data.liquidations?.cascade && (
        <span
          className="badge"
          title={`Каскад ликвидаций: $${(data.liquidations.total1hUSD / 1_000_000).toFixed(1)}M за последний час — повышенный риск резких движений`}
          style={{ background: 'rgba(239, 68, 68, 0.15)', color: 'var(--danger)', border: '1px solid rgba(239, 68, 68, 0.3)' }}
        >
          🚨 Ликвидации: ${(data.liquidations.total1hUSD / 1_000_000).toFixed(1)}M/час
        </span>
      )}
    </div>
  );
}
