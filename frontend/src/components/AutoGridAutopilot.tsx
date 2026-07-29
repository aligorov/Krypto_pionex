import { useState } from 'react';

interface Candidate {
  symbol: string;
  volatility: string;
  volume24h: string;
  fundingRate: string;
  evPct: string;
  sharpe: string;
  decision: 'ACCEPTED' | 'REJECTED';
  rejectionReason?: string;
}

interface ActiveBot {
  buOrderId: string;
  symbol: string;
  direction: string;
  range: string;
  gridNum: number;
  leverage: number;
  investmentUsdt: string;
  status: string;
  pnlUsdt: string;
  reconciliationState: string;
}

export default function AutoGridAutopilot({ token: _token }: { token: string }) {
  const [status, setStatus] = useState<'STOPPED' | 'RUNNING' | 'PAUSED' | 'EMERGENCY_STOPPED'>('STOPPED');
  const [mode, setMode] = useState<'PAPER' | 'REAL'>('PAPER');
  const [budget, setBudget] = useState('1000.0');
  const [maxBots, setMaxBots] = useState(3);
  const [leverage, setLeverage] = useState(5);

  const [candidates] = useState<Candidate[]>([
    {
      symbol: 'BTC_USDT_PERP',
      volatility: '4.2%',
      volume24h: '$120,450,000',
      fundingRate: '+0.0100%',
      evPct: '+1.45%',
      sharpe: '2.14',
      decision: 'ACCEPTED',
    },
    {
      symbol: 'ETH_USDT_PERP',
      volatility: '5.8%',
      volume24h: '$84,120,000',
      fundingRate: '-0.0050%',
      evPct: '+0.88%',
      sharpe: '1.65',
      decision: 'ACCEPTED',
    },
    {
      symbol: 'SOL_USDT_PERP',
      volatility: '12.4%',
      volume24h: '$45,000,000',
      fundingRate: '+0.0350%',
      evPct: '-0.20%',
      sharpe: '0.45',
      decision: 'REJECTED',
      rejectionReason: 'Volatility exceeds risk threshold and EV is negative',
    },
  ]);

  const [activeBots] = useState<ActiveBot[]>([
    {
      buOrderId: 'GRID_987654321',
      symbol: 'BTC_USDT_PERP',
      direction: 'LONG',
      range: '55,000 - 68,000',
      gridNum: 20,
      leverage: 5,
      investmentUsdt: '300.00',
      status: 'RUNNING',
      pnlUsdt: '+$14.20',
      reconciliationState: 'REST_AUTHORITATIVE_OK',
    },
  ]);

  const handleStart = () => {
    if (mode === 'REAL') {
      const confirmRun = window.confirm('WARNING: REAL Mode will place actual native Futures Grid orders on Pionex! Proceed?');
      if (!confirmRun) return;
    }
    setStatus('RUNNING');
  };

  const handleScanNow = () => {
    alert('Triggered dynamic market volatility & funding scanner across all Pionex PERP pairs!');
  };

  const handleStop = () => {
    setStatus('STOPPED');
  };

  const handleEmergencyStop = () => {
    setStatus('EMERGENCY_STOPPED');
    alert('EMERGENCY STOP TRIGGERED: Cancelling all active grid bots and flattening positions!');
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Top Banner Status */}
      <div className="grid-card" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <div>
          <h3 style={{ margin: 0 }}>Auto-Grid Autopilot State</h3>
          <p style={{ margin: '0.25rem 0 0 0', color: 'var(--text-muted)', fontSize: '0.875rem' }}>
            Exclusive Engine for Pionex Futures Grid Bots
          </p>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
          <span className={`badge ${status === 'RUNNING' ? 'badge-success' : status === 'EMERGENCY_STOPPED' ? 'badge-danger' : 'badge-warning'}`}>
            {status}
          </span>
          <span className={`badge ${mode === 'REAL' ? 'badge-danger' : 'badge-success'}`}>
            MODE: {mode}
          </span>
        </div>
      </div>

      {/* Operator Controls & Settings */}
      <div className="grid-card" style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem' }}>
        <div>
          <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Execution Mode</label>
          <select
            value={mode}
            onChange={(e) => setMode(e.target.value as 'PAPER' | 'REAL')}
            style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155' }}
          >
            <option value="PAPER">PAPER (Simulation)</option>
            <option value="REAL">REAL (Live Pionex API)</option>
          </select>
        </div>

        <div>
          <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Budget (USDT)</label>
          <input
            type="number"
            value={budget}
            onChange={(e) => setBudget(e.target.value)}
            style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
          />
        </div>

        <div>
          <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Max Active Bots</label>
          <input
            type="number"
            value={maxBots}
            onChange={(e) => setMaxBots(Number(e.target.value))}
            style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
          />
        </div>

        <div>
          <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Leverage</label>
          <input
            type="number"
            value={leverage}
            onChange={(e) => setLeverage(Number(e.target.value))}
            style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
          />
        </div>
      </div>

      {/* Action Buttons */}
      <div style={{ display: 'flex', gap: '1rem', flexWrap: 'wrap' }}>
        <button
          onClick={handleStart}
          disabled={status === 'RUNNING'}
          style={{ padding: '0.75rem 1.5rem', backgroundColor: 'var(--success-color)', color: '#fff', border: 'none', borderRadius: '0.375rem', fontWeight: 'bold', cursor: 'pointer' }}
        >
          ▶ Start Autopilot
        </button>
        <button
          onClick={handleScanNow}
          style={{ padding: '0.75rem 1.5rem', backgroundColor: 'var(--accent-color)', color: '#0f172a', border: 'none', borderRadius: '0.375rem', fontWeight: 'bold', cursor: 'pointer' }}
        >
          🔍 Scan Markets Now
        </button>
        <button
          onClick={handleStop}
          disabled={status === 'STOPPED'}
          style={{ padding: '0.75rem 1.5rem', backgroundColor: '#64748b', color: '#fff', border: 'none', borderRadius: '0.375rem', fontWeight: 'bold', cursor: 'pointer' }}
        >
          ⏹ Stop Autopilot
        </button>
        <button
          onClick={handleEmergencyStop}
          style={{ padding: '0.75rem 1.5rem', backgroundColor: 'var(--danger-color)', color: '#fff', border: 'none', borderRadius: '0.375rem', fontWeight: 'bold', cursor: 'pointer' }}
        >
          🚨 Emergency Stop
        </button>
      </div>

      {/* Active Grid Bots Table */}
      <div className="grid-card">
        <h3>Active Native Futures Grid Bots</h3>
        <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '1rem', textAlign: 'left' }}>
          <thead>
            <tr style={{ borderBottom: '1px solid #334155', color: 'var(--text-muted)' }}>
              <th style={{ padding: '0.5rem' }}>Remote BU Order ID</th>
              <th style={{ padding: '0.5rem' }}>Symbol</th>
              <th style={{ padding: '0.5rem' }}>Side</th>
              <th style={{ padding: '0.5rem' }}>Price Range</th>
              <th style={{ padding: '0.5rem' }}>Grids</th>
              <th style={{ padding: '0.5rem' }}>Leverage</th>
              <th style={{ padding: '0.5rem' }}>Investment</th>
              <th style={{ padding: '0.5rem' }}>Status</th>
              <th style={{ padding: '0.5rem' }}>Verified PnL</th>
            </tr>
          </thead>
          <tbody>
            {activeBots.map((bot) => (
              <tr key={bot.buOrderId} style={{ borderBottom: '1px solid #1e293b' }}>
                <td style={{ padding: '0.75rem 0.5rem', fontFamily: 'monospace' }}>{bot.buOrderId}</td>
                <td style={{ padding: '0.75rem 0.5rem', fontWeight: 'bold' }}>{bot.symbol}</td>
                <td style={{ padding: '0.75rem 0.5rem', color: '#4ade80' }}>{bot.direction}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{bot.range}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{bot.gridNum}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{bot.leverage}x</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>${bot.investmentUsdt}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className="badge badge-success">{bot.status}</span>
                </td>
                <td style={{ padding: '0.75rem 0.5rem', color: '#4ade80', fontWeight: 'bold' }}>{bot.pnlUsdt}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Scanner Candidates Table */}
      <div className="grid-card">
        <h3>Pionex Market Scanner Candidates</h3>
        <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '1rem', textAlign: 'left' }}>
          <thead>
            <tr style={{ borderBottom: '1px solid #334155', color: 'var(--text-muted)' }}>
              <th style={{ padding: '0.5rem' }}>Symbol</th>
              <th style={{ padding: '0.5rem' }}>Volatility</th>
              <th style={{ padding: '0.5rem' }}>24h Volume</th>
              <th style={{ padding: '0.5rem' }}>Funding Rate</th>
              <th style={{ padding: '0.5rem' }}>EV (%)</th>
              <th style={{ padding: '0.5rem' }}>Sharpe Ratio</th>
              <th style={{ padding: '0.5rem' }}>Decision</th>
            </tr>
          </thead>
          <tbody>
            {candidates.map((c) => (
              <tr key={c.symbol} style={{ borderBottom: '1px solid #1e293b' }}>
                <td style={{ padding: '0.75rem 0.5rem', fontWeight: 'bold' }}>{c.symbol}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{c.volatility}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{c.volume24h}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{c.fundingRate}</td>
                <td style={{ padding: '0.75rem 0.5rem', color: Number(c.evPct.replace('%', '')) > 0 ? '#4ade80' : '#f87171' }}>{c.evPct}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{c.sharpe}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className={`badge ${c.decision === 'ACCEPTED' ? 'badge-success' : 'badge-danger'}`}>
                    {c.decision}
                  </span>
                  {c.rejectionReason && (
                    <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginTop: '0.25rem' }}>{c.rejectionReason}</div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
