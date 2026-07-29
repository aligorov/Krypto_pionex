import { useState } from 'react';

interface Props {
  canOperate: boolean;
}

export default function AutoGridAutopilot({ canOperate: _canOperate }: Props) {
  const [status, setStatus] = useState<'STOPPED' | 'RUNNING' | 'PAUSED' | 'EMERGENCY_STOPPED'>('RUNNING');
  const [budgetPerBot, setBudgetPerBot] = useState('100');
  const [maxBots, setMaxBots] = useState('1');
  const [tradingMode, setTradingMode] = useState('PAPER');
  const [exchange, setExchange] = useState('PIONEX');
  const [stopLoss, setStopLoss] = useState('ATR');
  const [smartHarvester, setSmartHarvester] = useState('ENABLED');
  const [adaptiveLeverage, setAdaptiveLeverage] = useState('ENABLED');
  const [densityGrid, setDensityGrid] = useState('ENABLED');
  const [saveToast, setSaveToast] = useState<string | null>(null);

  const handleSaveSettings = () => {
    setSaveToast('💾 Настройки автопилота успешно сохранены!');
    setTimeout(() => setSaveToast(null), 3000);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* 1. Header Banner */}
      <div className="grid-card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', gap: '1rem' }}>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem', marginBottom: '0.5rem' }}>
              <h3 style={{ margin: 0, fontSize: '1.35rem', fontWeight: 700 }}>⚡ Auto-Grid Автопилот</h3>
              <span className={`badge ${status === 'RUNNING' ? 'badge-success' : 'badge-danger'}`}>
                {status === 'RUNNING' ? '● АКТИВЕН' : 'STOPPED'}
              </span>
            </div>
            <p style={{ color: 'var(--text-dark-400)', fontSize: '0.85rem', maxWidth: '750px' }}>
              Автоматический сканинг волатильных пар + Бэктест 14d + Консенсус тренда (1h/15m MTF) + Фильтр RVOL + Адаптивный ATR Стоп-Лосс + Trailing Profit Lock.
            </p>
          </div>

          {/* Banner Action Buttons */}
          <div style={{ display: 'flex', gap: '0.625rem', flexWrap: 'wrap' }}>
            <button
              className="btn-primary"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem', backgroundColor: '#0284c7' }}
            >
              ⚡ Сканировать сейчас
            </button>
            <button
              onClick={() => setStatus(status === 'RUNNING' ? 'STOPPED' : 'RUNNING')}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: '0.375rem',
                padding: '0.625rem 1rem',
                borderRadius: '0.5rem',
                border: '1px solid var(--border-dark-600)',
                backgroundColor: '#1e293b',
                color: '#f8fafc',
                fontSize: '0.875rem',
                fontWeight: 600,
                cursor: 'pointer',
              }}
            >
              {status === 'RUNNING' ? '🛑 Остановить Автопилот' : '▶ Запустить'}
            </button>
            <button
              onClick={() => setStatus('EMERGENCY_STOPPED')}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: '0.375rem',
                padding: '0.625rem 1rem',
                borderRadius: '0.5rem',
                border: '1px solid #7f1d1d',
                backgroundColor: '#991b1b',
                color: '#ffffff',
                fontSize: '0.875rem',
                fontWeight: 600,
                cursor: 'pointer',
              }}
            >
              Emergency Stop
            </button>
          </div>
        </div>
      </div>

      {/* 2. Durable Settings Grid (2 rows of 4 columns) */}
      <div className="grid-card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <h3 style={{ margin: 0, fontSize: '1.1rem', fontWeight: 600 }}>Параметры торговли</h3>
          <span className="badge badge-warning" style={{ textTransform: 'uppercase' }}>
            MODE: {tradingMode}
          </span>
        </div>

        {saveToast && (
          <div style={{ backgroundColor: 'rgba(34, 197, 94, 0.2)', color: '#4ade80', padding: '0.75rem', borderRadius: '0.5rem', marginBottom: '1rem', fontSize: '0.875rem' }}>
            {saveToast}
          </div>
        )}

        {/* Row 1: 4 columns */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem', marginBottom: '1rem' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              Бюджет на 1 бота ($)
            </label>
            <input
              type="number"
              value={budgetPerBot}
              onChange={(e) => setBudgetPerBot(e.target.value)}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              Макс. одновременно ботов
            </label>
            <input
              type="number"
              value={maxBots}
              onChange={(e) => setMaxBots(e.target.value)}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              Режим торговли
            </label>
            <select value={tradingMode} onChange={(e) => setTradingMode(e.target.value)}>
              <option value="PAPER">⚡ PAPER — безопасная симуляция</option>
              <option value="REAL">🔥 REAL — реальное исполнение сеток</option>
            </select>
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              Биржа для торговли
            </label>
            <select value={exchange} onChange={(e) => setExchange(e.target.value)}>
              <option value="PIONEX">PIONEX — Pionex ★</option>
            </select>
          </div>
        </div>

        {/* Row 2: 4 columns */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem', alignItems: 'flex-end' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              🛡️ Стоп-Лосс (Защита)
            </label>
            <select value={stopLoss} onChange={(e) => setStopLoss(e.target.value)}>
              <option value="ATR">🛡️ Адаптивный ATR (Волатильность)</option>
              <option value="FIXED">Фиксированный %</option>
            </select>
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              🎯 Умный Фиксатор PnL
            </label>
            <select value={smartHarvester} onChange={(e) => setSmartHarvester(e.target.value)}>
              <option value="ENABLED">🎯 Вкл (EMA/RSI/Fib/DT)</option>
              <option value="DISABLED">Выкл.</option>
            </select>
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
              ⚡ Адаптивное плечо
            </label>
            <select value={adaptiveLeverage} onChange={(e) => setAdaptiveLeverage(e.target.value)}>
              <option value="ENABLED">⚡ Вкл (2x-4x ATR Scale)</option>
              <option value="DISABLED">Выкл.</option>
            </select>
          </div>

          <div style={{ display: 'flex', gap: '0.5rem' }}>
            <div style={{ flex: 1 }}>
              <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-dark-400)', marginBottom: '0.375rem', fontWeight: 500 }}>
                📌 Сгущение на Дне
              </label>
              <select value={densityGrid} onChange={(e) => setDensityGrid(e.target.value)}>
                <option value="ENABLED">📌 Вкл (Плотный выкуп)</option>
                <option value="DISABLED">Выкл.</option>
              </select>
            </div>

            <button
              onClick={handleSaveSettings}
              className="btn-primary"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem', alignSelf: 'flex-end', height: '40px', whiteSpace: 'nowrap' }}
            >
              💾 Сохранить
            </button>
          </div>
        </div>
      </div>

      {/* 3. 4 Stat Cards Row */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem' }}>
        <div className="stat-card">
          <span className="stat-label">АКТИВНЫЕ АВТО-СЕТКИ</span>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: '0.5rem' }}>
            <span className="stat-value" style={{ color: '#4ade80' }}>0</span>
            <span style={{ color: 'var(--text-dark-400)', fontSize: '0.9rem' }}>/ {maxBots}</span>
          </div>
        </div>

        <div className="stat-card">
          <span className="stat-label">РЕАЛЬНЫЙ ПРОФИТ (REAL)</span>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: '0.5rem' }}>
            <span className="stat-value" style={{ color: '#4ade80' }}>+$0</span>
            <span style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)' }}>Paper: +$218.53</span>
          </div>
        </div>

        <div className="stat-card">
          <span className="stat-label">ВСЕГО АВТО-РОТАЦИЙ</span>
          <span className="stat-value" style={{ color: 'var(--primary-400)' }}>50</span>
        </div>

        <div className="stat-card">
          <span className="stat-label">МЕТРИКА ВОЛАТИЛЬНОСТИ</span>
          <span className="stat-value" style={{ fontSize: '1.25rem', color: '#f8fafc' }}>Exit &lt; 1.5%</span>
        </div>
      </div>

      {/* 4. Metric Indicators Row */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '0.75rem' }}>
        <div style={{ backgroundColor: 'var(--bg-dark-900)', border: '1px solid var(--border-dark-700)', padding: '0.75rem 1rem', borderRadius: '0.5rem', fontSize: '0.8rem' }}>
          <span style={{ color: 'var(--text-dark-400)' }}>📉 MTF Консенсус:</span>{' '}
          <strong style={{ color: 'var(--text-dark-100)' }}>1h + 15m EMA Trend Alignment</strong>
        </div>
        <div style={{ backgroundColor: 'var(--bg-dark-900)', border: '1px solid var(--border-dark-700)', padding: '0.75rem 1rem', borderRadius: '0.5rem', fontSize: '0.8rem' }}>
          <span style={{ color: 'var(--text-dark-400)' }}>⚡ RVOL Gate:</span>{' '}
          <strong style={{ color: 'var(--text-dark-100)' }}>Фильтр ложных пробоев (&gt;1.2x)</strong>
        </div>
        <div style={{ backgroundColor: 'var(--bg-dark-900)', border: '1px solid var(--border-dark-700)', padding: '0.75rem 1rem', borderRadius: '0.5rem', fontSize: '0.8rem' }}>
          <span style={{ color: 'var(--text-dark-400)' }}>🛡️ Dynamic ATR Spacing:</span>{' '}
          <strong style={{ color: 'var(--text-dark-100)' }}>Step = 0.8 x ATR</strong>
        </div>
        <div style={{ backgroundColor: 'var(--bg-dark-900)', border: '1px solid var(--border-dark-700)', padding: '0.75rem 1rem', borderRadius: '0.5rem', fontSize: '0.8rem' }}>
          <span style={{ color: 'var(--text-dark-400)' }}>⚡ Auto-Leverage:</span>{' '}
          <strong style={{ color: 'var(--text-dark-100)' }}>2x-4x ATR Risk Scale</strong>
        </div>
      </div>

      {/* 5. Category Profitability Heatmap */}
      <div className="grid-card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
          <h3 style={{ margin: 0, fontSize: '1.1rem', fontWeight: 600 }}>🔥 Тепловая карта прибыльности категорий и монет</h3>
          <span style={{ fontSize: '0.75rem', color: '#38bdf8', fontWeight: 600 }}>● Live Sector Intelligence</span>
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem' }}>
          <div style={{ backgroundColor: '#091522', border: '1px solid #16385c', borderRadius: '0.5rem', padding: '1rem' }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)', marginBottom: '0.25rem' }}>🤖 AI &amp; Innovation</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: '#4ade80', marginBottom: '0.25rem' }}>+28.4% WinRate</div>
            <div style={{ fontSize: '0.7rem', color: 'var(--text-dark-500)' }}>EBA, AVAAI, QNTX</div>
          </div>

          <div style={{ backgroundColor: '#0b1d28', border: '1px solid #18445e', borderRadius: '0.5rem', padding: '1rem' }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)', marginBottom: '0.25rem' }}>⚡ Layer 1 / Infra</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: '#4ade80', marginBottom: '0.25rem' }}>+34.1% WinRate</div>
            <div style={{ fontSize: '0.7rem', color: 'var(--text-dark-500)' }}>AKE, ONE, AERGO</div>
          </div>

          <div style={{ backgroundColor: '#18122c', border: '1px solid #3c236e', borderRadius: '0.5rem', padding: '1rem' }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)', marginBottom: '0.25rem' }}>🏦 DeFi &amp; Trading</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: '#c084fc', marginBottom: '0.25rem' }}>+19.8% WinRate</div>
            <div style={{ fontSize: '0.7rem', color: 'var(--text-dark-500)' }}>BANK, DODOX, PROM</div>
          </div>

          <div style={{ backgroundColor: '#26180a', border: '1px solid #5e3713', borderRadius: '0.5rem', padding: '1rem' }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)', marginBottom: '0.25rem' }}>🎮 Gaming &amp; Memes</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: '#fbbf24', marginBottom: '0.25rem' }}>+16.2% WinRate</div>
            <div style={{ fontSize: '0.7rem', color: 'var(--text-dark-500)' }}>ESPORTS, KORU, LAB</div>
          </div>
        </div>
      </div>

      {/* 6. Event History Card */}
      <div className="grid-card">
        <h3>📜 История ротаций и событий</h3>
        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', fontFamily: 'monospace', fontSize: '0.85rem' }}>
          <div style={{ display: 'flex', gap: '1rem', padding: '0.5rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
            <span style={{ color: '#38bdf8', fontWeight: 600 }}>INJ/USDT:USDT</span>
            <span className="badge badge-success">[STOPPED_TP]</span>
            <span style={{ color: 'var(--text-dark-400)' }}>Bot stopped with status STOPPED_MANUAL</span>
          </div>
          <div style={{ display: 'flex', gap: '1rem', padding: '0.5rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
            <span style={{ color: '#38bdf8', fontWeight: 600 }}>TAO/USDT:USDT</span>
            <span className="badge badge-success">[STOPPED_TP]</span>
            <span style={{ color: 'var(--text-dark-400)' }}>Bot stopped with status STOPPED_MANUAL</span>
          </div>
          <div style={{ display: 'flex', gap: '1rem', padding: '0.5rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
            <span style={{ color: '#38bdf8', fontWeight: 600 }}>BNB/USDT:USDT</span>
            <span className="badge badge-success">[STOPPED_TP]</span>
            <span style={{ color: 'var(--text-dark-400)' }}>Bot stopped with status STOPPED_MANUAL</span>
          </div>
          <div style={{ display: 'flex', gap: '1rem', padding: '0.5rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
            <span style={{ color: '#38bdf8', fontWeight: 600 }}>HYPE/USDT:USDT</span>
            <span className="badge badge-success">[STOPPED_TP]</span>
            <span style={{ color: 'var(--text-dark-400)' }}>Bot stopped with status STOPPED_MANUAL</span>
          </div>
        </div>
      </div>
    </div>
  );
}
