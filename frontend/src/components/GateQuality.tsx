import { useCallback, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { GateQualityRow } from '../types';

// «Качество гейтов» (план §7, экран 3): сводка 24ч/7д по режимам —
// гейт → эпизоды → покрытие shadow → модельные ±$ заблокированных входов →
// достоверность (proof strength). Агрегаты считает воркер пакетами; экран
// только читает сохранённые строки.

const REGIME_LABELS: Record<string, { text: string; cls: string }> = {
  RANGE: { text: 'RANGE', cls: 'badge neutral' },
  TREND_UP: { text: 'TREND_UP', cls: 'badge success' },
  TREND_DOWN: { text: 'TREND_DOWN', cls: 'badge danger' },
  UNKNOWN: { text: 'UNKNOWN', cls: 'badge warning' },
};

const PROOF_STYLES: Record<string, { cls: string; title: string }> = {
  HIGH: { cls: 'badge success', title: 'Покрытие достаточное, исходы завершены — выводу можно доверять' },
  MEDIUM: { cls: 'badge warning', title: 'Данные есть, но покрытие/завершённость неполные' },
  LOW: { cls: 'badge danger', title: 'Тонкое покрытие или открытые исходы — вывода пока нет' },
};

function fmtSignedUsd(value: string | null): { text: string; positive: boolean } {
  if (value === null || value === '') return { text: '—', positive: false };
  const num = Number(value);
  if (!Number.isFinite(num)) return { text: value, positive: false };
  return { text: `${num > 0 ? '+' : ''}${num.toFixed(2)} $`, positive: num > 0 };
}

export default function GateQuality() {
  const [window, setWindow] = useState<'24H' | '7D'>('24H');
  const [rows, setRows] = useState<GateQualityRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [computedAt, setComputedAt] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api<{ window: string; data: GateQualityRow[] }>(
        `/api/autogrid/gate-quality?window=${window}`,
      );
      setRows(res.data ?? []);
      setError(null);
      const latest = (res.data ?? [])
        .map((row) => row.computedAt)
        .filter(Boolean)
        .sort()
        .pop();
      setComputedAt(latest ?? null);
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, [window]);

  useEffect(() => {
    setRows(null);
    void load();
  }, [load]);

  return (
    <div className="panel">
      <div className="panel-heading" style={{ flexWrap: 'wrap', gap: '1rem' }}>
        <div>
          <span className="eyebrow">ОТЧЁТ</span>
          <h3>Качество гейтов</h3>
          <small className="muted" style={{ display: 'block', marginTop: '2px' }}>
            По режимам рынка: эпизоды, покрытие shadow-наблюдениями, модельные ±$ заблокированных входов
            {computedAt ? ` · пересчитано ${new Date(computedAt).toLocaleString()}` : ''}
          </small>
        </div>
        <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
          <button
            className={`button small ${window === '24H' ? 'primary' : ''}`}
            onClick={() => setWindow('24H')}
          >
            24ч
          </button>
          <button
            className={`button small ${window === '7D' ? 'primary' : ''}`}
            onClick={() => setWindow('7D')}
          >
            7 дней
          </button>
          <button className="button small" onClick={() => void load()}>Обновить</button>
        </div>
      </div>

      {error && <div className="alert danger" style={{ marginBottom: '0.75rem' }}>{error}</div>}

      {rows === null && !error && <small className="muted">Загрузка агрегатов…</small>}
      {rows !== null && rows.length === 0 && (
        <div className="empty-state">
          Агрегатов за окно {window} ещё нет. Воркер считает их пакетами (~раз в 20ч) —
          первые достоверные цифры появляются после цикла созревания траекторий (24ч).
        </div>
      )}

      {rows !== null && rows.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Гейт</th>
                <th>Режим</th>
                <th>Эпизоды</th>
                <th>Решения</th>
                <th title="Доля отказов, по которым реально есть shadow-наблюдение (знаменатель — coverage-журнал)">Покрытие</th>
                <th title="Завершённые / открытые исходы shadow-эпизодов">Исходы</th>
                <th title="Модельный ±$ заблокированных входов при фиксированной инвестиции">Модельные ±$</th>
                <th title="Сила доказательства: покрытие + завершённость исходов">Достоверность</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const regime = REGIME_LABELS[row.regime] ?? { text: row.regime, cls: 'badge neutral' };
                const proof = PROOF_STYLES[row.proofStrength] ?? PROOF_STYLES.LOW;
                const pnl = fmtSignedUsd(row.blockedModelPnl);
                const coveragePct = row.coverage === null ? null : Number(row.coverage) * 100;
                return (
                  <tr key={`${row.window}-${row.windowStart}-${row.gate}-${row.regime}`}>
                    <td><strong>{row.gate}</strong></td>
                    <td><span className={regime.cls}>{regime.text}</span></td>
                    <td>{row.episodes}</td>
                    <td>{row.decisions}</td>
                    <td
                      title={Object.keys(row.skipReasons ?? {}).length > 0 ? `Пропуски: ${JSON.stringify(row.skipReasons)}` : ''}
                    >
                      {coveragePct === null || !Number.isFinite(coveragePct)
                        ? '—'
                        : `${coveragePct.toFixed(1)}%`}
                    </td>
                    <td>
                      <small>
                        <span title="Завершённые исходы">{row.completedOutcomes}</span>
                        {' / '}
                        <span title="Открытые исходы" className="muted">{row.openOutcomes}</span>
                      </small>
                    </td>
                    <td style={{ color: pnl.positive ? '#34d399' : undefined }}>
                      <strong style={{ color: row.blockedModelPnl !== null && !pnl.positive && pnl.text !== '—' ? '#f87171' : undefined }}>
                        {pnl.text}
                      </strong>
                    </td>
                    <td><span className={proof.cls} title={proof.title}>{row.proofStrength}</span></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
