import { useState, useEffect } from 'react';

type Tab = 'dashboard' | 'accounts' | 'grid' | 'patterns' | 'risk' | 'ai' | 'telegram' | 'audit';

interface RiskSettings {
  KillSwitchEnabled: boolean;
  MaxAccountExposureUSD: string;
  MaxLeverage: number;
}

export default function App() {
  const [activeTab, setActiveTab] = useState<Tab>('dashboard');
  const [riskSettings, setRiskSettings] = useState<RiskSettings | null>(null);

  useEffect(() => {
    fetch('/api/risk/settings')
      .then((res) => res.json())
      .then((data) => setRiskSettings(data))
      .catch(() => setRiskSettings({ KillSwitchEnabled: true, MaxAccountExposureUSD: '1000.0', MaxLeverage: 10 }));
  }, []);

  return (
    <div>
      <header className="app-header">
        <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
          <h2 style={{ margin: 0, color: 'var(--accent-color)' }}>Pionex Bot Dashboard</h2>
          <span className={`badge ${riskSettings?.KillSwitchEnabled ? 'badge-danger' : 'badge-success'}`}>
            {riskSettings?.KillSwitchEnabled ? 'KILL SWITCH: ACTIVE' : 'SYSTEM: READY'}
          </span>
        </div>
        <nav className="nav-links">
          {(['dashboard', 'accounts', 'grid', 'patterns', 'risk', 'ai', 'telegram', 'audit'] as Tab[]).map((tab) => (
            <button
              key={tab}
              className={`nav-btn ${activeTab === tab ? 'active' : ''}`}
              onClick={() => setActiveTab(tab)}
            >
              {tab.toUpperCase()}
            </button>
          ))}
        </nav>
      </header>

      <main className="main-container">
        {activeTab === 'dashboard' && (
          <div className="grid-card">
            <h3>System Status</h3>
            <p><strong>Exclusive Provider:</strong> Pionex REST & Futures WebSocket API</p>
            <p><strong>Backend Version:</strong> 1.0.0 (Production)</p>
            <p><strong>Runtime Config:</strong> PostgreSQL (Zero-ENV Policy Active)</p>
          </div>
        )}

        {activeTab === 'grid' && (
          <div className="grid-card">
            <h3>Native Futures Grid Bot Lifecycle</h3>
            <p>Creates native grid bots via <code>/api/v1/bot/futuresGrid/create</code> and verifies remote <code>buOrderId</code> before setting state to RUNNING.</p>
          </div>
        )}

        {activeTab === 'risk' && (
          <div className="grid-card">
            <h3>Durable Risk Engine</h3>
            <p><strong>Kill Switch State:</strong> {riskSettings?.KillSwitchEnabled ? 'ON (Blocked)' : 'OFF (Allowed)'}</p>
            <p><strong>Max Account Exposure:</strong> ${riskSettings?.MaxAccountExposureUSD} USD</p>
            <p><strong>Max Leverage:</strong> {riskSettings?.MaxLeverage}x</p>
          </div>
        )}

        {activeTab === 'ai' && (
          <div className="grid-card">
            <h3>Pionex AI Boundary</h3>
            <p>Pionex Spot AI strategies are strictly isolated and never applied to Futures Grid bots. All AI recommendations require explicit approval before execution.</p>
          </div>
        )}
      </main>
    </div>
  );
}
