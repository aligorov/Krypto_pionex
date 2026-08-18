import { useEffect, useState } from 'react';
import { api } from '../api';

interface FundingResponse {
  data: {
    averageRate: number;
    binance: number;
    bybit: number;
    okx: number;
    isExtreme: boolean;
  };
}

interface FundingState {
  loading: boolean;
  rate: number | null;
  isExtreme: boolean;
}

const REFRESH_MS = 60_000;
// Below 0.001%/8h the funding is noise — render as neutral.
const NEUTRAL_RATE = 0.00001;

function formatRate(rate: number): string {
  return `${(Math.abs(rate) * 100).toFixed(3)}%`;
}

/**
 * FundingBadge — cross-exchange funding badge for a candidate row.
 * Positive funding: longs pay shorts (SHORT earns) → green.
 * Negative funding: shorts pay longs (LONG earns) → green for long grids.
 * Extreme |funding| (>0.1%/8h) overrides to red. Silent on API failure.
 */
export function FundingBadge({ symbol }: { symbol: string }) {
  const [state, setState] = useState<FundingState>({ loading: true, rate: null, isExtreme: false });

  useEffect(() => {
    const load = async () => {
      try {
        const res = await api<FundingResponse>(
          `/api/market/funding?symbol=${encodeURIComponent(symbol)}`,
        );
        setState({ loading: false, rate: res.data.averageRate, isExtreme: Boolean(res.data.isExtreme) });
      } catch {
        // Silent degradation: an unavailable feed must not break the table.
        setState((current) => ({ ...current, loading: false }));
      }
    };
    void load();
    const timer = window.setInterval(() => void load(), REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [symbol]);

  if (state.loading) {
    return <small className="muted">…</small>;
  }
  if (state.rate === null) {
    return (
      <small className="muted" title="Нет свежих данных фандинга (за 10 минут)">
        —
      </small>
    );
  }

  const rate = state.rate;
  const earns = rate >= 0 ? 'SHORT' : 'LONG';
  const color = state.isExtreme ? 'var(--danger)' : 'var(--accent)';
  const background = state.isExtreme ? 'rgba(239, 68, 68, 0.15)' : 'rgba(16, 185, 129, 0.12)';
  const border = state.isExtreme ? 'rgba(239, 68, 68, 0.3)' : 'rgba(16, 185, 129, 0.3)';
  const title = [
    `Средний фандинг (Binance/Bybit/OKX): ${(rate * 100).toFixed(4)}% / 8ч`,
    rate >= 0 ? 'LONG платят, SHORT зарабатывают' : 'SHORT платят, LONG зарабатывают',
    state.isExtreme ? '⚠ Экстремальный фандинг (>0.1%/8ч)' : null,
  ]
    .filter(Boolean)
    .join(' · ');

  if (Math.abs(rate) < NEUTRAL_RATE && !state.isExtreme) {
    return (
      <span className="badge neutral" title={title}>
        ○ {formatRate(rate)}
      </span>
    );
  }

  return (
    <span className="badge" title={title} style={{ background, color, border: `1px solid ${border}` }}>
      {state.isExtreme && '⚠ '}
      {earns} earn {formatRate(rate)}
    </span>
  );
}
