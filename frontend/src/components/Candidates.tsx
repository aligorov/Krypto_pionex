import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, getCachedAutoGrid, setCachedAutoGrid } from '../api';
import { describeError } from './AutoGridAutopilot';
import { CandlestickChart } from './CandlestickChart';
import type { AIKitResponse, AutoGridCandidate, AutoGridState } from '../types';

interface Props {
  canOperate: boolean;
}

interface AIKitState {
  loading: boolean;
  data: AIKitResponse | null;
  error: string | null;
}

type SortKey = 'createdAt' | 'score' | 'volatilityPct' | 'expectedValuePct' | 'symbol';
type SortOrder = 'asc' | 'desc';

export default function Candidates({ canOperate: _canOperate }: Props) {
  const [state, setState] = useState<AutoGridState | null>(() => getCachedAutoGrid<AutoGridState>());
  const [error, setError] = useState<string | null>(null);
  const [aiKit, setAiKit] = useState<Record<string, AIKitState>>({});
  const [sortKey, setSortKey] = useState<SortKey>('createdAt');
  const [sortOrder, setSortOrder] = useState<SortOrder>('desc');
  const [selectedForChart, setSelectedForChart] = useState<AutoGridCandidate | null>(null);
  const [lastSyncAt, setLastSyncAt] = useState<Date | null>(null);
  const [syncError, setSyncError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const result = await api<AutoGridState>('/api/autogrid');
      setState(result);
      setCachedAutoGrid(result);
      setLastSyncAt(new Date());
      setSyncError(null);
    } catch (loadError) {
      // Silent cache fallback must stay VISIBLE: a frozen table with no
      // warning reads as "no new candidates" while the truth is "data not
      // refreshing".
      setSyncError(describeError(loadError));
      if (!getCachedAutoGrid()) {
        setError(describeError(loadError));
      }
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 15000);
    return () => window.clearInterval(timer);
  }, [load]);

  async function fetchAIKit(symbol: string) {
    setAiKit((current) => ({ ...current, [symbol]: { loading: true, data: null, error: null } }));
    try {
      const data = await api<AIKitResponse>(
        `/api/autogrid/ai-strategy?symbol=${encodeURIComponent(symbol)}`,
      );
      setAiKit((current) => ({ ...current, [symbol]: { loading: false, data, error: null } }));
    } catch (fetchError) {
      setAiKit((current) => ({
        ...current,
        [symbol]: { loading: false, data: null, error: describeError(fetchError) },
      }));
    }
  }

  function handleSort(key: SortKey) {
    if (sortKey === key) {
      setSortOrder((prev) => (prev === 'desc' ? 'asc' : 'desc'));
    } else {
      setSortKey(key);
      setSortOrder('desc');
    }
  }

  const sortCandidates = useCallback((candidates: AutoGridCandidate[]) => {
    return [...candidates].sort((a, b) => {
      let comparison = 0;
      switch (sortKey) {
        case 'createdAt':
          comparison = new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime();
          break;
        case 'score':
          comparison = Number(a.score) - Number(b.score);
          break;
        case 'volatilityPct':
          comparison = Number(a.volatilityPct) - Number(b.volatilityPct);
          break;
        case 'expectedValuePct':
          comparison = Number(a.expectedValuePct) - Number(b.expectedValuePct);
          break;
        case 'symbol':
          comparison = a.symbol.localeCompare(b.symbol);
          break;
      }
      return sortOrder === 'desc' ? -comparison : comparison;
    });
  }, [sortKey, sortOrder]);

  const accepted = useMemo(() => {
    if (!state) return [];
    return sortCandidates(state.candidates.filter((c) => c.decision === 'ACCEPTED'));
  }, [state, sortCandidates]);

  const rejected = useMemo(() => {
    if (!state) return [];
    return sortCandidates(state.candidates.filter((c) => c.decision !== 'ACCEPTED'));
  }, [state, sortCandidates]);

  if (!state) {
    return <div className="empty-state">{error ?? 'Загрузка кандидатов…'}</div>;
  }

  const scan = state.lastScan;

  return (
    <div className="section-stack">
      {error && (
        <div className="alert danger">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      {/* Chart Modal */}
      {selectedForChart && (
        <div
          style={{
            position: 'fixed',
            inset: 0,
            backgroundColor: 'rgba(0, 0, 0, 0.75)',
            zIndex: 1000,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            padding: '24px',
          }}
          onClick={() => setSelectedForChart(null)}
        >
          <div
            style={{
              width: '1000px',
              maxWidth: '95vw',
              height: '600px',
              maxHeight: '90vh',
              borderRadius: '12px',
              overflow: 'hidden',
              boxShadow: '0 25px 50px -12px rgba(0, 0, 0, 0.5)',
            }}
            onClick={(e) => e.stopPropagation()}
          >
            <CandlestickChart
              symbol={selectedForChart.symbol}
              lowerPrice={Number(selectedForChart.lowerPrice)}
              upperPrice={Number(selectedForChart.upperPrice)}
              currentPrice={Number(selectedForChart.currentPrice)}
              gridCount={selectedForChart.gridNum}
              direction={selectedForChart.recommendedTrend?.toUpperCase() || 'NEUTRAL'}
              onClose={() => setSelectedForChart(null)}
            />
          </div>
        </div>
      )}

      {syncError && (
        <div className="banner error" style={{ marginBottom: '0.75rem' }}>
          ⚠ Данные не обновляются{lastSyncAt ? ` (последняя успешная загрузка ${lastSyncAt.toLocaleTimeString()})` : ''}: {syncError}
        </div>
      )}
      {lastSyncAt && !syncError && new Date().getTime() - lastSyncAt.getTime() > 60000 && (
        <div className="banner error" style={{ marginBottom: '0.75rem' }}>
          ⚠ Экран давно не обновлялся ({lastSyncAt.toLocaleTimeString()}) — обнови страницу (Ctrl+Shift+R)
        </div>
      )}

      <div className="panel">
        <div className="panel-heading" style={{ flexWrap: 'wrap', gap: '1rem' }}>
          <div>
            <span className="eyebrow">LAST SCAN</span>
            <h3>Кандидаты последнего скана ({accepted.length})</h3>
            <small className="muted" style={{ display: 'block', marginTop: '2px' }}>
              {state?.lastScan?.completedAt
                ? `Скан завершён ${new Date(state.lastScan.completedAt).toLocaleTimeString()}`
                : 'Скан ещё не завершался'}
              {lastSyncAt ? ` · экран обновлён ${lastSyncAt.toLocaleTimeString()}` : ''}
            </small>
          </div>

          <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.35rem', fontSize: '0.85rem' }}>
              <span className="muted">Сортировка:</span>
              <button
                className={`button small ${sortKey === 'createdAt' ? 'primary' : ''}`}
                onClick={() => handleSort('createdAt')}
                title="Сортировать по времени сканирования"
              >
                Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
              <button
                className={`button small ${sortKey === 'score' ? 'primary' : ''}`}
                onClick={() => handleSort('score')}
                title="Сортировать по Score"
              >
                Score {sortKey === 'score' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
              <button
                className={`button small ${sortKey === 'volatilityPct' ? 'primary' : ''}`}
                onClick={() => handleSort('volatilityPct')}
                title="Сортировать по волатильности"
              >
                Волатильность {sortKey === 'volatilityPct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
            </div>

            <span className="muted">
              {scan
                ? `${new Date(scan.startedAt).toLocaleString()} · ${scan.candidatesFound} пар`
                : 'Скан ещё не выполнялся'}
            </span>
          </div>
        </div>

        {accepted.length === 0 ? (
          <div className="empty-state">
            Принятых кандидатов нет. Запустите автопилот или сделайте скан на экране «Автопилот».
          </div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th onClick={() => handleSort('createdAt')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th onClick={() => handleSort('symbol')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Символ {sortKey === 'symbol' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th onClick={() => handleSort('score')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Score {sortKey === 'score' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>Режим рынка</th>
                  <th>Конгльюэнс</th>
                  <th>Позиция в диапазоне</th>
                  <th>Радар входа (Снайпер)</th>
                  <th onClick={() => handleSort('volatilityPct')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Волатильность {sortKey === 'volatilityPct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>ADX</th>
                  <th>Грид (диапазон)</th>
                  <th>Направление</th>
                  <th>Плечо</th>
                  <th onClick={() => handleSort('expectedValuePct')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    EV / Sharpe {sortKey === 'expectedValuePct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>AI Kit</th>
                  <th>График</th>
                </tr>
              </thead>
              <tbody>
                {accepted.map((candidate) => (
                  <CandidateRow
                    key={candidate.id}
                    candidate={candidate}
                    aiKit={aiKit[candidate.symbol]}
                    onFetchAIKit={() => void fetchAIKit(candidate.symbol)}
                    onOpenChart={() => setSelectedForChart(candidate)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {rejected.length > 0 && (
        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">REJECTED</span>
              <h3>Отклонённые ({rejected.length})</h3>
            </div>
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th onClick={() => handleSort('createdAt')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>Символ</th>
                  <th>Причина</th>
                  <th>Волатильность</th>
                  <th>Объём 24ч</th>
                  <th>График</th>
                </tr>
              </thead>
              <tbody>
                {rejected.map((candidate) => (
                  <tr key={candidate.id}>
                    <td>
                      <span className="badge neutral" title={new Date(candidate.createdAt).toLocaleString()}>
                        {formatTime(candidate.createdAt)}
                      </span>
                    </td>
                    <td><strong>{candidate.symbol}</strong></td>
                    <td>
                      {candidate.rejectionReason?.startsWith('AI:') ? (
                        <span style={{ color: '#f87171' }}>🧠 {candidate.rejectionReason}</span>
                      ) : (
                        <small>{candidate.rejectionReason ?? '—'}</small>
                      )}
                    </td>
                    <td>{candidate.volatilityPct}%</td>
                    <td>{candidate.volume24h}</td>
                    <td>
                      <button
                        className="button small"
                        onClick={() => setSelectedForChart(candidate)}
                        title="Открыть интерактивный график"
                      >
                        📊
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}

function CandidateRow({
  candidate,
  aiKit,
  onFetchAIKit,
  onOpenChart,
}: {
  candidate: AutoGridCandidate;
  aiKit?: AIKitState;
  onFetchAIKit: () => void;
  onOpenChart: () => void;
}) {
  const assumptions = candidate.modelAssumptions as Record<string, unknown>;
  const regime = String(assumptions['regime'] ?? '—');
  const adx = assumptions['adx'];
  const rangePosition = assumptions['rangePositionPct'];
  const storedAI = assumptions['aiKit'] as Record<string, unknown> | undefined;

  return (
    <tr>
      <td>
        <span className="badge neutral" title={new Date(candidate.createdAt).toLocaleString()}>
          {formatTime(candidate.createdAt)}
        </span>
      </td>
      <td>
        <strong style={{ cursor: 'pointer', color: '#38bdf8' }} onClick={onOpenChart} title="Открыть график">
          {candidate.symbol}
        </strong>
        <small>{candidate.currentPrice}</small>
        {assumptions['llmConfidence'] !== undefined && (
          <div style={{ marginTop: '3px' }}>
            <span
              className="badge"
              style={{ background: 'rgba(59, 130, 246, 0.15)', color: '#60a5fa', border: '1px solid rgba(59, 130, 246, 0.3)', fontSize: '10px', padding: '1px 5px', cursor: 'help' }}
              title={`🧠 AI Мозг (${assumptions['llmRegime'] || 'Аудит'}): ${assumptions['llmReasoning'] || ''}`}
            >
              🧠 AI {Math.round(Number(assumptions['llmConfidence']) * 100)}%
            </span>
          </div>
        )}
      </td>
      <td><strong>{Number(candidate.score).toFixed(3)}</strong></td>
      <td>
        <span className={`badge ${regimeBadge(regime)}`}>{regime}</span>
      </td>
      <td>
        <ConfluenceBadge assumptions={assumptions} />
      </td>
      <td>{formatNumber(rangePosition)}%</td>
      <td>
        {candidate.recommendedTrend === 'long' ? (
          Number(rangePosition) > 40 ? (
            <span className="badge" style={{ background: 'rgba(239, 68, 68, 0.15)', color: '#f87171', border: '1px solid rgba(239, 68, 68, 0.3)' }}>
              🚫 НА ХАЯХ
            </span>
          ) : (
            <span className="badge" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#34d399', border: '1px solid rgba(16, 185, 129, 0.3)' }}>
              🎯 ВХОД (Дно {formatNumber(rangePosition)}%)
            </span>
          )
        ) : candidate.recommendedTrend === 'short' ? (
          Number(rangePosition) < 60 ? (
            <span className="badge" style={{ background: 'rgba(239, 68, 68, 0.15)', color: '#f87171', border: '1px solid rgba(239, 68, 68, 0.3)' }}>
              🚫 НА ДНЕ
            </span>
          ) : (
            <span className="badge" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#34d399', border: '1px solid rgba(16, 185, 129, 0.3)' }}>
              🎯 ВХОД (Пик {formatNumber(rangePosition)}%)
            </span>
          )
        ) : (
          <span className="badge" style={{ background: 'rgba(56, 189, 248, 0.15)', color: '#38bdf8', border: '1px solid rgba(56, 189, 248, 0.3)' }}>
            🟢 Ядро канала ({formatNumber(rangePosition)}%)
          </span>
        )}
      </td>
      <td>{Number(candidate.volatilityPct).toFixed(2)}%</td>
      <td>{formatNumber(adx)}</td>
      <td>
        {candidate.lowerPrice} – {candidate.upperPrice}
        <small>{candidate.gridNum} уровней</small>
      </td>
      <td>
        <span className={`badge ${candidate.recommendedTrend === 'long' ? 'success' : candidate.recommendedTrend === 'short' ? 'danger' : 'neutral'}`}>
          {candidate.recommendedTrend}
        </span>
      </td>
      <td>{candidate.recommendedLeverage}x</td>
      <td>
        {Number(candidate.expectedValuePct).toFixed(3)}% / {Number(candidate.sharpe).toFixed(2)}
      </td>
      <td>
        {storedAI ? (
          <div>
            <span className="badge success">AI Kit</span>
            <small>
              год. {String(storedAI['annualized'])} · волат. {String(storedAI['volatility'])}
              {storedAI['gridCount'] ? ` · сеток: ${String(storedAI['gridCount'])} (AI)` : ''}
            </small>
          </div>
        ) : aiKit?.loading ? (
          <small>Запрос AI Kit…</small>
        ) : aiKit?.data ? (
          <div>
            <span className="badge success">AI Kit</span>
            <small>
              год. {aiKit.data.strategy.annualized} · волат. {aiKit.data.strategy.volatility} ·{' '}
              {aiKit.data.strategy.gridCount} ур.
            </small>
          </div>
        ) : (
          <div>
            <button className="button small" onClick={onFetchAIKit}>AI Kit</button>
            {aiKit?.error && <small>{aiKit.error}</small>}
          </div>
        )}
      </td>
      <td>
        <button
          className="button small"
          onClick={onOpenChart}
          title="Открыть график со слоем сетки"
        >
          📊
        </button>
      </td>
    </tr>
  );
}

function regimeBadge(regime: string): string {
  if (regime === 'TREND_UP') return 'success';
  if (regime === 'TREND_DOWN') return 'danger';
  if (regime === 'RANGE') return 'neutral';
  return 'warning';
}

function formatNumber(value: unknown): string {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return '—';
  return parsed.toFixed(1);
}

function formatTime(dateStr: string): string {
  if (!dateStr) return '—';
  const d = new Date(dateStr);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}


const CONFLUENCE_LABELS: Record<string, { text: string; color: string; bg: string; border: string }> = {
  SUPPORT_LONG: { text: '▲ LONG', color: '#34d399', bg: 'rgba(16, 185, 129, 0.15)', border: 'rgba(16, 185, 129, 0.3)' },
  SUPPORT_SHORT: { text: '▼ SHORT', color: '#f87171', bg: 'rgba(239, 68, 68, 0.15)', border: 'rgba(239, 68, 68, 0.3)' },
  SUPPORT_RANGE: { text: '◆ RANGE', color: '#60a5fa', bg: 'rgba(59, 130, 246, 0.15)', border: 'rgba(59, 130, 246, 0.3)' },
  CONFLICT: { text: '⚔ CONFLICT', color: '#fbbf24', bg: 'rgba(245, 158, 11, 0.15)', border: 'rgba(245, 158, 11, 0.3)' },
  NEUTRAL: { text: '○ NEUTRAL', color: '#94a3b8', bg: 'rgba(148, 163, 184, 0.12)', border: 'rgba(148, 163, 184, 0.3)' },
};

function ConfluenceBadge({ assumptions }: { assumptions: Record<string, unknown> }) {
  const confluence = assumptions['confluence'] as Record<string, unknown> | undefined;
  const verdictRaw = confluence?.['verdict'];
  if (typeof verdictRaw !== 'string') {
    return <span className="muted">—</span>;
  }
  const verdict = verdictRaw as keyof typeof CONFLUENCE_LABELS;
  const style = CONFLUENCE_LABELS[verdict] ?? CONFLUENCE_LABELS.NEUTRAL;
  const strength = Number(confluence?.['strength'] ?? 0);
  const hurst = assumptions['hurst'];
  const gate = confluence?.['hurstGate'];
  const title = [
    `Конгльюэнс: ${verdict} (сила ${(strength * 100).toFixed(0)}%)`,
    typeof hurst === 'number' ? `Hurst: ${hurst.toFixed(3)}` : null,
    typeof gate === 'string' ? `Режим памяти: ${gate}` : null,
    `Голоса: long ${Number(confluence?.['longScore'] ?? 0).toFixed(2)} / short ${Number(confluence?.['shortScore'] ?? 0).toFixed(2)} / range ${Number(confluence?.['rangeScore'] ?? 0).toFixed(2)}`,
  ].filter(Boolean).join('\n');
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '3px', alignItems: 'flex-start' }}>
      <span
        className="badge"
        style={{ background: style.bg, color: style.color, border: `1px solid ${style.border}`, cursor: 'help' }}
        title={title}
      >
        {style.text} {(strength * 100).toFixed(0)}%
      </span>
      {typeof hurst === 'number' && (
        <small
          className="muted"
          style={{ cursor: 'help' }}
          title="Hurst (DFA): <0.45 возврат к среднему, >0.58 трендовая опасность для сетки"
        >
          H {hurst.toFixed(2)}
        </small>
      )}
    </div>
  );
}
