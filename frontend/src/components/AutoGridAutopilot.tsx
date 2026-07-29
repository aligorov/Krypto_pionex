import { useState } from 'react';

interface Props {
  canOperate: boolean;
}

export default function AutoGridAutopilot({ canOperate: _canOperate }: Props) {
  const [status, setStatus] = useState<'STOPPED' | 'RUNNING' | 'PAUSED' | 'EMERGENCY_STOPPED'>('STOPPED');
  const [budgetPerBot, setBudgetPerBot] = useState('100');
  const [maxBots, setMaxBots] = useState('1');
  const [tradingMode, setTradingMode] = useState('PAPER');
  const [exchange, setExchange] = useState('PIONEX');
  const [stopLoss, setStopLoss] = useState('ADAPTIVE');
  const [smartHarvester, setSmartHarvester] = useState('OFF');
  const [adaptiveLeverage, setAdaptiveLeverage] = useState('ON');
  const [maxLeverage, setMaxLeverage] = useState('2');
  const [densityGrid, setDensityGrid] = useState('OFF');
  const [timeframe, setTimeframe] = useState('15M');
  const [modelCandles, setModelCandles] = useState('192');
  const [scanPairs, setScanPairs] = useState('12');

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Banner Card */}
      <div className="grid-card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', gap: '1rem' }}>
          <div style={{ display: 'flex', gap: '1rem' }}>
            <div style={{ width: '48px', height: '48px', borderRadius: '0.625rem', backgroundColor: '#162c20', border: '1px solid #059669', display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: '1.25rem' }}>
              ⚡
            </div>
            <div>
              <div style={{ fontSize: '0.75rem', fontWeight: 700, color: '#34d399', letterSpacing: '0.05em', marginBottom: '0.25rem' }}>
                PIONEX NATIVE FUTURES GRID
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <h2 style={{ margin: 0, fontSize: '1.5rem', fontWeight: 700, color: 'var(--text-bright)' }}>
                  Auto-Grid Автопилот
                </h2>
                <span className="badge" style={{ backgroundColor: '#241d11', color: '#fbbf24', border: '1px solid #b45309' }}>
                  {status}
                </span>
              </div>
              <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem', marginTop: '0.5rem', maxWidth: '650px' }}>
                Pionex PERP scanner → модельные EV/Sharpe/Sortino/DD/PF → risk pre-flight → PAPER simulator или native Futures Grid lifecycle.
              </p>
            </div>
          </div>

          {/* Action Buttons */}
          <div style={{ display: 'flex', gap: '0.625rem', flexWrap: 'wrap' }}>
            <button
              onClick={() => setStatus('RUNNING')}
              className="btn-launch"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem' }}
            >
              ▶ Запустить
            </button>
            <button
              className="btn-scan"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem' }}
            >
              🔆 Сканировать сейчас
            </button>
            <button
              onClick={() => setStatus('STOPPED')}
              className="btn-stop"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem' }}
            >
              ■ Остановить Автопилот
            </button>
            <button
              onClick={() => setStatus('EMERGENCY_STOPPED')}
              className="btn-emergency"
              style={{ display: 'flex', alignItems: 'center', gap: '0.375rem' }}
            >
              Emergency Stop
            </button>
          </div>
        </div>
      </div>

      {/* Durable Settings Grid (3 rows of 4 columns) */}
      <div className="grid-card">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.25rem' }}>
          <div>
            <div style={{ fontSize: '0.7rem', fontWeight: 700, color: '#34d399', letterSpacing: '0.05em', marginBottom: '0.25rem' }}>
              DURABLE SETTINGS
            </div>
            <h3 style={{ margin: 0, fontSize: '1.2rem', fontWeight: 700, color: 'var(--text-bright)' }}>
              Параметры торговли
            </h3>
          </div>
          <span className="badge badge-killswitch">
            MODE: {tradingMode}
          </span>
        </div>

        {/* Row 1 */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1.25rem', marginBottom: '1.25rem' }}>
          <div>
            <label>Бюджет на 1 бота (USDT)</label>
            <input type="number" value={budgetPerBot} onChange={(e) => setBudgetPerBot(e.target.value)} />
          </div>
          <div>
            <label>Макс. одновременно ботов</label>
            <input type="number" value={maxBots} onChange={(e) => setMaxBots(e.target.value)} />
          </div>
          <div>
            <label>Режим торговли</label>
            <select value={tradingMode} onChange={(e) => setTradingMode(e.target.value)}>
              <option value="PAPER">PAPER — безопасная симуляция</option>
              <option value="REAL">REAL — реальное исполнение сеток</option>
            </select>
          </div>
          <div>
            <label>Биржа / аккаунт</label>
            <select value={exchange} onChange={(e) => setExchange(e.target.value)}>
              <option value="PIONEX">PIONEX — выберите аккаунт</option>
            </select>
          </div>
        </div>

        {/* Row 2 */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1.25rem', marginBottom: '1.25rem' }}>
          <div>
            <label>Стоп-лосс (защита)</label>
            <select value={stopLoss} onChange={(e) => setStopLoss(e.target.value)}>
              <option value="ADAPTIVE">Адаптивный диапазон + native</option>
              <option value="FIXED">Фиксированный %</option>
            </select>
          </div>
          <div>
            <label>Умный фиксатор PnL</label>
            <select value={smartHarvester} onChange={(e) => setSmartHarvester(e.target.value)}>
              <option value="OFF">Выкл.</option>
              <option value="ON">Вкл.</option>
            </select>
          </div>
          <div>
            <label>Адаптивное плечо</label>
            <select value={adaptiveLeverage} onChange={(e) => setAdaptiveLeverage(e.target.value)}>
              <option value="ON">Вкл. — ограничение по volatility</option>
              <option value="OFF">Выкл.</option>
            </select>
          </div>
          <div>
            <label>Максимальное плечо</label>
            <input type="number" value={maxLeverage} onChange={(e) => setMaxLeverage(e.target.value)} />
          </div>
        </div>

        {/* Row 3 */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1.25rem' }}>
          <div>
            <label>Сгущение сетки</label>
            <select value={densityGrid} onChange={(e) => setDensityGrid(e.target.value)}>
              <option value="OFF">Выкл. — arithmetic grid</option>
              <option value="ON">Вкл.</option>
            </select>
          </div>
          <div>
            <label>Таймфрейм</label>
            <select value={timeframe} onChange={(e) => setTimeframe(e.target.value)}>
              <option value="15M">15M</option>
              <option value="1H">1H</option>
              <option value="4H">4H</option>
            </select>
          </div>
          <div>
            <label>Свечей в модели</label>
            <input type="number" value={modelCandles} onChange={(e) => setModelCandles(e.target.value)} />
          </div>
          <div>
            <label>Пар за один скап</label>
            <input type="number" value={scanPairs} onChange={(e) => setScanPairs(e.target.value)} />
          </div>
        </div>
      </div>
    </div>
  );
}
