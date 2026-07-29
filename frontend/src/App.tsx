import { useState, useEffect } from 'react';
import PionexAccounts from './components/PionexAccounts';
import AutoGridAutopilot from './components/AutoGridAutopilot';
import TelegramNotifications from './components/TelegramNotifications';
import AuditLogs from './components/AuditLogs';
import MCPServer from './components/MCPServer';
import SettingsView from './components/SettingsView';

type Tab = 'dashboard' | 'accounts' | 'autogrid' | 'telegram' | 'audit' | 'mcp' | 'settings';

interface RiskSettings {
  KillSwitchEnabled: boolean;
  MaxAccountExposureUSD: string;
  MaxLeverage: number;
}

export default function App() {
  const [token, setToken] = useState<string | null>(localStorage.getItem('pionex_token'));
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [loginError, setLoginError] = useState<string | null>(null);

  const [activeTab, setActiveTab] = useState<Tab>('dashboard');
  const [riskSettings, setRiskSettings] = useState<RiskSettings | null>(null);

  useEffect(() => {
    if (!token) return;

    fetch('/api/risk/settings', {
      headers: {
        Authorization: `Bearer ${token}`,
      },
    })
      .then((res) => {
        if (res.status === 401) {
          handleLogout();
          return null;
        }
        return res.json();
      })
      .then((data) => {
        if (data) setRiskSettings(data);
      })
      .catch(() => {
        setRiskSettings({ KillSwitchEnabled: true, MaxAccountExposureUSD: '1000.0', MaxLeverage: 10 });
      });
  }, [token]);

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoginError(null);

    try {
      const res = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      });

      const data = await res.json();
      if (!res.ok) {
        setLoginError(data.error || 'Login failed');
        return;
      }

      localStorage.setItem('pionex_token', data.token);
      setToken(data.token);
    } catch {
      setLoginError('Server connection failed');
    }
  };

  const handleLogout = () => {
    localStorage.removeItem('pionex_token');
    setToken(null);
  };

  if (!token) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', backgroundColor: 'var(--bg-color)' }}>
        <div className="grid-card" style={{ width: '360px', padding: '2rem' }}>
          <h2 style={{ textAlign: 'center', color: 'var(--accent-color)', marginBottom: '1.5rem' }}>Pionex Bot Login</h2>
          {loginError && (
            <div style={{ backgroundColor: 'rgba(239, 68, 68, 0.2)', color: 'var(--danger-color)', padding: '0.75rem', borderRadius: '0.375rem', marginBottom: '1rem', textAlign: 'center' }}>
              {loginError}
            </div>
          )}
          <form onSubmit={handleLogin}>
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', color: 'var(--text-muted)', marginBottom: '0.5rem', fontSize: '0.875rem' }}>Username</label>
              <input
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="admin"
                required
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', border: '1px solid #334155', backgroundColor: '#0f172a', color: '#fff', boxSizing: 'border-box' }}
              />
            </div>
            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', color: 'var(--text-muted)', marginBottom: '0.5rem', fontSize: '0.875rem' }}>Password</label>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="pionex2026"
                required
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', border: '1px solid #334155', backgroundColor: '#0f172a', color: '#fff', boxSizing: 'border-box' }}
              />
            </div>
            <button
              type="submit"
              style={{ width: '100%', padding: '0.75rem', borderRadius: '0.375rem', border: 'none', backgroundColor: 'var(--accent-color)', color: '#0f172a', fontWeight: 'bold', cursor: 'pointer' }}
            >
              Sign In
            </button>
          </form>
        </div>
      </div>
    );
  }

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
          {(['dashboard', 'accounts', 'autogrid', 'telegram', 'audit', 'mcp', 'settings'] as Tab[]).map((tab) => (
            <button
              key={tab}
              className={`nav-btn ${activeTab === tab ? 'active' : ''}`}
              onClick={() => setActiveTab(tab)}
            >
              {tab === 'autogrid' ? 'AUTO-GRID' : tab === 'audit' ? 'AUDIT & LOGS' : tab.toUpperCase()}
            </button>
          ))}
          <button className="nav-btn" onClick={handleLogout} style={{ color: 'var(--danger-color)', borderColor: 'var(--danger-color)' }}>
            LOGOUT
          </button>
        </nav>
      </header>

      <main className="main-container">
        {activeTab === 'dashboard' && (
          <div className="grid-card">
            <h3>System Status</h3>
            <p><strong>Exclusive Provider:</strong> Pionex REST & Futures WebSocket API</p>
            <p><strong>Backend Version:</strong> 1.0.0 (Production)</p>
            <p><strong>Runtime Config:</strong> PostgreSQL (Zero-ENV Policy Active)</p>
            <p><strong>Session Status:</strong> Authenticated (RBAC Active)</p>
          </div>
        )}

        {activeTab === 'accounts' && <PionexAccounts canManage={true} />}

        {activeTab === 'autogrid' && <AutoGridAutopilot canOperate={true} />}

        {activeTab === 'telegram' && <TelegramNotifications token={token} />}

        {activeTab === 'audit' && <AuditLogs token={token} />}

        {activeTab === 'mcp' && <MCPServer token={token} />}

        {activeTab === 'settings' && <SettingsView token={token} />}
      </main>
    </div>
  );
}
