import { useState, useEffect } from 'react';
import PionexAccounts from './components/PionexAccounts';
import AutoGridAutopilot from './components/AutoGridAutopilot';
import TelegramNotifications from './components/TelegramNotifications';
import AuditLogs from './components/AuditLogs';
import MCPServer from './components/MCPServer';
import SettingsView from './components/SettingsView';

type Tab = 'dashboard' | 'accounts' | 'autogrid' | 'risk' | 'telegram' | 'audit' | 'mcp' | 'settings';

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

  const [activeTab, setActiveTab] = useState<Tab>('autogrid');
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
      <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', backgroundColor: 'var(--bg-dark-950)' }}>
        <div className="grid-card" style={{ width: '380px', padding: '2rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', gap: '0.5rem', marginBottom: '1.5rem' }}>
            <span style={{ fontSize: '1.75rem' }}>🤖</span>
            <h2 style={{ margin: 0, color: 'var(--primary-400)', fontSize: '1.5rem', fontWeight: 700 }}>CryptoBot</h2>
          </div>
          {loginError && (
            <div style={{ backgroundColor: 'rgba(239, 68, 68, 0.2)', color: 'var(--danger-red)', padding: '0.75rem', borderRadius: '0.5rem', marginBottom: '1rem', textAlign: 'center', fontSize: '0.875rem' }}>
              {loginError}
            </div>
          )}
          <form onSubmit={handleLogin}>
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', color: 'var(--text-dark-400)', marginBottom: '0.5rem', fontSize: '0.85rem' }}>Username</label>
              <input
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="admin"
                required
              />
            </div>
            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', color: 'var(--text-dark-400)', marginBottom: '0.5rem', fontSize: '0.85rem' }}>Password</label>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="pionex2026"
                required
              />
            </div>
            <button
              type="submit"
              className="btn-primary"
              style={{ width: '100%', padding: '0.75rem' }}
            >
              Sign In
            </button>
          </form>
        </div>
      </div>
    );
  }

  const sidebarItems: { id: Tab; label: string; icon: string }[] = [
    { id: 'dashboard', label: 'Analysis', icon: '📊' },
    { id: 'autogrid', label: 'Trade', icon: '📈' },
    { id: 'risk', label: 'Risk', icon: '🛡️' },
    { id: 'accounts', label: 'Portfolio', icon: '💼' },
    { id: 'telegram', label: 'Alerts', icon: '🔔' },
    { id: 'audit', label: 'Logs', icon: '📜' },
    { id: 'mcp', label: 'MCP Server', icon: '⚡' },
    { id: 'settings', label: 'Settings', icon: '⚙️' },
  ];

  return (
    <div style={{ display: 'flex', minHeight: '100vh', backgroundColor: 'var(--bg-dark-950)' }}>
      {/* Left Sidebar Navigation */}
      <aside style={{ width: '240px', backgroundColor: 'var(--bg-dark-900)', borderRight: '1px solid var(--border-dark-700)', display: 'flex', flexDirection: 'column', justifyContent: 'space-between', padding: '1.25rem 1rem' }}>
        <div>
          {/* Logo Header */}
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.625rem', marginBottom: '2rem', paddingLeft: '0.5rem' }}>
            <span style={{ fontSize: '1.5rem' }}>🤖</span>
            <h1 style={{ margin: 0, fontSize: '1.25rem', fontWeight: 700, color: 'var(--text-dark-100)' }}>CryptoBot</h1>
          </div>

          {/* Navigation Links */}
          <nav style={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
            {sidebarItems.map((item) => (
              <button
                key={item.id}
                onClick={() => setActiveTab(item.id)}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: '0.75rem',
                  padding: '0.625rem 0.875rem',
                  borderRadius: '0.5rem',
                  fontSize: '0.875rem',
                  fontWeight: 500,
                  color: activeTab === item.id ? 'var(--primary-400)' : 'var(--text-dark-400)',
                  backgroundColor: activeTab === item.id ? 'rgba(2, 132, 199, 0.18)' : 'transparent',
                  border: activeTab === item.id ? '1px solid rgba(2, 132, 199, 0.4)' : '1px solid transparent',
                  cursor: 'pointer',
                  textAlign: 'left',
                  transition: 'all 0.15s ease-in-out',
                }}
              >
                <span>{item.icon}</span>
                <span>{item.label}</span>
              </button>
            ))}
          </nav>
        </div>

        {/* Bottom User Status */}
        <div style={{ borderTop: '1px solid var(--border-dark-700)', paddingTop: '1rem', marginTop: '1rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.75rem', paddingLeft: '0.5rem' }}>
            <span style={{ width: '8px', height: '8px', borderRadius: '50%', backgroundColor: 'var(--success-green)' }}></span>
            <span style={{ fontSize: '0.75rem', color: 'var(--text-dark-400)' }}>Connected aligorov (admin)</span>
          </div>
          <button
            onClick={handleLogout}
            style={{
              width: '100%',
              padding: '0.5rem',
              borderRadius: '0.5rem',
              border: '1px solid var(--border-dark-600)',
              backgroundColor: 'transparent',
              color: 'var(--danger-red)',
              fontSize: '0.85rem',
              fontWeight: 600,
              cursor: 'pointer',
            }}
          >
            Logout
          </button>
        </div>
      </aside>

      {/* Main Content Area */}
      <main style={{ flex: 1, padding: '1.75rem 2rem', overflowY: 'auto' }}>
        {/* Top Status Header */}
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
            <h2 style={{ margin: 0, fontSize: '1.5rem', fontWeight: 700, color: 'var(--text-dark-100)' }}>
              Pionex Bot Control Plane
            </h2>
            <span className={`badge ${riskSettings?.KillSwitchEnabled ? 'badge-danger' : 'badge-success'}`}>
              {riskSettings?.KillSwitchEnabled ? 'KILL SWITCH: ACTIVE' : 'SYSTEM: READY'}
            </span>
          </div>
        </div>

        {activeTab === 'dashboard' && (
          <div className="grid-card">
            <h3>System Overview</h3>
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
