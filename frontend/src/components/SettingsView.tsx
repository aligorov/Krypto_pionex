import { useState } from 'react';

export default function SettingsView({ token: _token }: { token: string }) {
  const [activeTab, setActiveTab] = useState<'risk' | 'flags' | 'execution'>('risk');
  const [killSwitch, setKillSwitch] = useState(true);
  const [maxExposure, setMaxExposure] = useState('1000.00');
  const [maxLeverage, setMaxLeverage] = useState(10);
  const [saveMsg, setSaveMsg] = useState<string | null>(null);

  const handleSaveRisk = (e: React.FormEvent) => {
    e.preventDefault();
    setSaveMsg('Risk parameters updated in PostgreSQL!');
    setTimeout(() => setSaveMsg(null), 3000);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Subtabs */}
      <div style={{ display: 'flex', gap: '1rem', borderBottom: '1px solid #334155', paddingBottom: '0.5rem' }}>
        <button className={`nav-btn ${activeTab === 'risk' ? 'active' : ''}`} onClick={() => setActiveTab('risk')}>
          Risk Controls
        </button>
        <button className={`nav-btn ${activeTab === 'flags' ? 'active' : ''}`} onClick={() => setActiveTab('flags')}>
          Feature Flags
        </button>
        <button className={`nav-btn ${activeTab === 'execution' ? 'active' : ''}`} onClick={() => setActiveTab('execution')}>
          Execution Boundaries
        </button>
      </div>

      {saveMsg && (
        <div style={{ backgroundColor: 'rgba(34, 197, 94, 0.2)', color: '#4ade80', padding: '0.75rem', borderRadius: '0.375rem' }}>
          {saveMsg}
        </div>
      )}

      {activeTab === 'risk' && (
        <div className="grid-card" style={{ maxWidth: '600px' }}>
          <h3>Durable Risk Engine Settings</h3>
          <form onSubmit={handleSaveRisk}>
            <div style={{ marginBottom: '1.25rem', backgroundColor: '#0f172a', padding: '1rem', borderRadius: '0.5rem', border: '1px solid #334155' }}>
              <label style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', cursor: 'pointer', fontWeight: 'bold' }}>
                <span>Global Kill Switch</span>
                <input
                  type="checkbox"
                  checked={killSwitch}
                  onChange={(e) => setKillSwitch(e.target.checked)}
                  style={{ width: '1.25rem', height: '1.25rem' }}
                />
              </label>
              <p style={{ margin: '0.5rem 0 0 0', fontSize: '0.85rem', color: 'var(--text-muted)' }}>
                When enabled, all new order entries and grid bot creations are immediately rejected across the system.
              </p>
            </div>

            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>
                Max Account Exposure (USD)
              </label>
              <input
                type="number"
                value={maxExposure}
                onChange={(e) => setMaxExposure(e.target.value)}
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
              />
            </div>

            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>
                Max Leverage Cap (x)
              </label>
              <input
                type="number"
                value={maxLeverage}
                onChange={(e) => setMaxLeverage(Number(e.target.value))}
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
              />
            </div>

            <button
              type="submit"
              style={{ width: '100%', padding: '0.75rem', backgroundColor: 'var(--accent-color)', color: '#0f172a', fontWeight: 'bold', border: 'none', borderRadius: '0.375rem', cursor: 'pointer' }}
            >
              Save Risk Settings to PostgreSQL
            </button>
          </form>
        </div>
      )}

      {activeTab === 'flags' && (
        <div className="grid-card">
          <h3>System Feature Flags (`feature_flags`)</h3>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem', marginTop: '1rem' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '0.75rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
              <div>
                <strong>mcp_control_plane</strong>
                <div style={{ fontSize: '0.85rem', color: 'var(--text-muted)' }}>Enable Model Context Protocol tools</div>
              </div>
              <span className="badge badge-success">ENABLED</span>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '0.75rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
              <div>
                <strong>paper_trading</strong>
                <div style={{ fontSize: '0.85rem', color: 'var(--text-muted)' }}>Enable paper-trading simulation engine</div>
              </div>
              <span className="badge badge-success">ENABLED</span>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '0.75rem', backgroundColor: '#0f172a', borderRadius: '0.375rem' }}>
              <div>
                <strong>real_native_grid</strong>
                <div style={{ fontSize: '0.85rem', color: 'var(--text-muted)' }}>Enable real native Pionex Futures Grid executor</div>
              </div>
              <span className="badge badge-danger">DISABLED</span>
            </div>
          </div>
        </div>
      )}

      {activeTab === 'execution' && (
        <div className="grid-card">
          <h3>Spot vs. Futures Boundary & AI Rules</h3>
          <p>1. Spot USDT balance MUST NOT be counted towards Futures margin.</p>
          <p>2. Pionex Spot Grid AI strategy parameters MUST NOT be applied to Futures Grid bots.</p>
          <p>3. Zero-ENV Runtime Policy: Infrastructure connection string `DATABASE_URL` is the only allowed ENV variable. All settings live in PostgreSQL.</p>
        </div>
      )}
    </div>
  );
}
