import { useCallback, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { EntryDecision, GateTraceEntry } from '../types';

// «История решения» (план §7, экран 1): последние entry_decisions по символу —
// исход, решающий гейт, стадия, качество данных; раскрытие строки показывает
// трассу гейт→вердикт→порог→входы и судьбу shadow-эпизода.
// Журнал исторический — опрос не нужен, только ручное обновление.

const OUTCOME_BADGES: Record<string, string> = {
  ALLOW: 'badge success',
  WAIT: 'badge warning',
  REJECT: 'badge danger',
};

const VERDICT_STYLES: Record<string, { label: string; color: string; bg: string; border: string }> = {
  PASS: { label: 'PASS', color: '#34d399', bg: 'rgba(16, 185, 129, 0.15)', border: 'rgba(16, 185, 129, 0.3)' },
  REJECT: { label: 'REJECT', color: '#f87171', bg: 'rgba(239, 68, 68, 0.15)', border: 'rgba(239, 68, 68, 0.3)' },
  WAIT: { label: 'WAIT', color: '#fbbf24', bg: 'rgba(245, 158, 11, 0.15)', border: 'rgba(245, 158, 11, 0.3)' },
  EXEMPT: { label: 'EXEMPT', color: '#facc15', bg: 'rgba(234, 179, 8, 0.15)', border: 'rgba(234, 179, 8, 0.3)' },
  NOT_EVALUATED: { label: '—', color: '#94a3b8', bg: 'rgba(148, 163, 184, 0.12)', border: 'rgba(148, 163, 184, 0.3)' },
  UNKNOWN: { label: '?', color: '#94a3b8', bg: 'rgba(148, 163, 184, 0.12)', border: 'rgba(148, 163, 184, 0.3)' },
};

const POS_STATE_LABELS: Record<string, string> = {
  OPEN: 'открыт',
  CLOSED_TP: 'закрыт по TP',
  CLOSED_SL: 'закрыт по SL',
  HORIZON_END: 'горизонт истёк',
  INVALIDATED: 'аннулирован',
};

const CALC_STATE_LABELS: Record<string, string> = {
  PENDING: 'ждёт расчёта',
  RUNNING: 'считается',
  DONE: 'готов',
  RETRYABLE_ERROR: 'ошибка (повтор)',
  INSUFFICIENT_DATA: 'мало данных',
};

function verdictStyle(verdict: string) {
  return VERDICT_STYLES[verdict] ?? VERDICT_STYLES.UNKNOWN;
}

function fmtPnl(value: string | null): string {
  if (value === null || value === '') return '—';
  const num = Number(value);
  if (!Number.isFinite(num)) return value;
  return `${num > 0 ? '+' : ''}${num.toFixed(2)} $`;
}

function pnlColor(value: string | null): string | undefined {
  const num = Number(value);
  if (!Number.isFinite(num) || num === 0) return undefined;
  return num > 0 ? '#34d399' : '#f87171';
}

function compactInputs(inputs: Record<string, unknown> | undefined): string {
  if (!inputs) return '';
  const parts = Object.entries(inputs)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .slice(0, 12)
    .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : String(v)}`);
  return parts.join(', ');
}

function TraceTable({ trace }: { trace: GateTraceEntry[] }) {
  if (trace.length === 0) {
    return <small className="muted">Трасса не записана (решение до v2.0.184 либо сбой записи — см. coverage)</small>;
  }
  return (
    <div className="table-wrap" style={{ margin: '6px -16px -12px' }}>
      <table>
        <thead>
          <tr>
            <th>Гейт</th>
            <th>Вердикт</th>
            <th>Порог</th>
            <th>Входы</th>
            <th>Качество</th>
          </tr>
        </thead>
        <tbody>
          {trace.map((entry, index) => {
            const style = verdictStyle(entry.verdict);
            return (
              <tr key={`${entry.gate}-${index}`}>
                <td><strong>{entry.gate}</strong>{entry.exemption ? <small className="muted" style={{ display: 'block' }}>искл.: {entry.exemption}</small> : null}</td>
                <td>
                  <span
                    className="badge"
                    style={{ background: style.bg, color: style.color, border: `1px solid ${style.border}` }}
                    title={entry.exemption ? `Исключение: ${entry.exemption}` : entry.verdict}
                  >
                    {style.label}
                  </span>
                </td>
                <td>{entry.threshold !== undefined && entry.threshold !== null && entry.threshold !== '' ? String(entry.threshold) : '—'}</td>
                <td style={{ maxWidth: '420px' }}>
                  <small className="muted" style={{ wordBreak: 'break-word' }}>{compactInputs(entry.inputs) || '—'}</small>
                </td>
                <td>{entry.quality ? <small>{entry.quality}</small> : '—'}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function ShadowFateView({ decision }: { decision: EntryDecision }) {
  const episode = decision.episode;
  if (!episode) {
    return <small className="muted">Эпизод не связан (запись до v2.0.184)</small>;
  }
  const shadow = episode.shadow;
  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: '10px', alignItems: 'center' }}>
      <span className="badge neutral" title={`Эпизод: попыток ${episode.attempts}, disposition ${episode.disposition}`}>
        эпизод · {episode.attempts} поп.
      </span>
      {shadow ? (
        <>
          <span
            className={`badge ${shadow.posState === 'CLOSED_TP' ? 'success' : shadow.posState === 'CLOSED_SL' ? 'danger' : 'neutral'}`}
            title={`Позиция shadow: ${shadow.posState}; расчёт: ${shadow.calcState}`}
          >
            shadow: {POS_STATE_LABELS[shadow.posState] ?? shadow.posState}
          </span>
          <small className="muted">{CALC_STATE_LABELS[shadow.calcState] ?? shadow.calcState}</small>
          {shadow.outcomePnlUsdt !== null && (
            <strong style={{ color: pnlColor(shadow.outcomePnlUsdt) }} title={shadow.outcomeReason ?? ''}>
              {fmtPnl(shadow.outcomePnlUsdt)}
            </strong>
          )}
          {shadow.outcomeReason && <small className="muted">{shadow.outcomeReason}</small>}
        </>
      ) : (
        <small className="muted">shadow-наблюдения нет (причина — в coverage-журнале)</small>
      )}
    </div>
  );
}

export default function DecisionHistoryPanel({ symbol, limit = 20 }: { symbol: string; limit?: number }) {
  const [decisions, setDecisions] = useState<EntryDecision[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [openId, setOpenId] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api<{ data: EntryDecision[] }>(
        `/api/autogrid/decisions?symbol=${encodeURIComponent(symbol)}&limit=${limit}`,
      );
      setDecisions(res.data ?? []);
      setError(null);
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, [symbol, limit]);

  useEffect(() => {
    setDecisions(null);
    setOpenId(null);
    void load();
  }, [load]);

  return (
    <div className="card-inset" style={{ margin: '6px 0', display: 'grid', gap: '10px' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '12px', flexWrap: 'wrap' }}>
        <div>
          <strong>История решений · {symbol}</strong>
          <small className="muted" style={{ display: 'block' }}>
            Последние {limit} попыток допуска: исход, решающий гейт, трасса и судьба shadow-эпизода
          </small>
        </div>
        <button className="button small" onClick={() => void load()}>Обновить</button>
      </div>

      {error && <div className="alert danger" style={{ margin: 0 }}>{error}</div>}

      {decisions === null && !error && <small className="muted">Загрузка журнала…</small>}
      {decisions !== null && decisions.length === 0 && (
        <small className="muted">Решений по символу ещё нет — журнал заполняется автопилотом во время сканов.</small>
      )}

      {decisions?.map((decision) => {
        const open = openId === decision.id;
        return (
          <div key={decision.id} style={{ borderTop: '1px solid var(--border)', paddingTop: '8px' }}>
            <div
              style={{ display: 'flex', flexWrap: 'wrap', gap: '8px', alignItems: 'center', cursor: 'pointer' }}
              onClick={() => setOpenId(open ? null : decision.id)}
            >
              <span className="badge neutral" title={new Date(decision.decisionAt ?? decision.createdAt).toLocaleString()}>
                {new Date(decision.decisionAt ?? decision.createdAt).toLocaleTimeString()}
              </span>
              <span className={OUTCOME_BADGES[decision.outcome] ?? 'badge neutral'}>{decision.outcome}</span>
              <strong title={decision.reason}>{decision.code}</strong>
              <span className="badge" title="Стадия геометрии на момент решения">{decision.stage}</span>
              <span
                className={`badge ${decision.dataQuality === 'OK' ? 'neutral' : 'warning'}`}
                title="Качество данных на момент решения (RV=0 при ошибке свечей ≠ отсутствие волатильности)"
              >
                {decision.dataQuality}
              </span>
              {decision.attemptNo > 1 && (
                <small className="muted" title="Попытка № внутри эпизода (символ+направление)">#{decision.attemptNo}</small>
              )}
              <small className="muted" style={{ flex: 1, minWidth: '120px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {decision.reason}
              </small>
              <small className="muted">{open ? '▲ свернуть' : '▼ трасса'}</small>
            </div>

            {open && (
              <div style={{ display: 'grid', gap: '10px', marginTop: '8px' }}>
                <TraceTable trace={decision.gateTrace ?? []} />
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: '14px', alignItems: 'center' }}>
                  <ShadowFateView decision={decision} />
                  {(decision.directionBefore || decision.directionAfter) && (
                    <small className="muted" title="Направление до/после смарт-override">
                      {decision.directionBefore ?? '—'} → {decision.directionAfter ?? '—'}
                    </small>
                  )}
                  {decision.priceAtDecision && (
                    <small className="muted" title={`Источник цены: ${decision.priceSource ?? '—'}`}>
                      цена {Number(decision.priceAtDecision).toPrecision(6)}
                    </small>
                  )}
                  <small className="muted">{decision.path} · {decision.fleet}</small>
                </div>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
