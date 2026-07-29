import { useState } from 'react';
import PionexAccounts from './components/PionexAccounts';
import AutoGridAutopilot from './components/AutoGridAutopilot';
import TelegramNotifications from './components/TelegramNotifications';
import AuditLogs from './components/AuditLogs';
import MCPServer from './components/MCPServer';
import SettingsView from './components/SettingsView';

type Tab = 'dashboard' | 'autogrid' | 'accounts' | 'gridbots' | 'risk' | 'audit' | 'settings' | 'telegram' | 'mcp';

export default function App() {
  const [token, setToken] = useState<string | null>(localStorage.getItem('pionex_token'));
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [loginError, setLoginError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const [activeTab, setActiveTab] = useState<Tab>('autogrid');
  const [alertDismissed, setAlertDismissed] = useState(false);

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoginError(null);
    setLoading(true);

    try {
      const res = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      });

      const data = await res.json();
      if (!res.ok) {
        setLoginError(data.error || 'Неверный логин или пароль');
        setLoading(false);
        return;
      }

      localStorage.setItem('pionex_token', data.token);
      setToken(data.token);
    } catch {
      setLoginError('Ошибка подключения к серверу');
    } finally {
      setLoading(false);
    }
  };

  const handleLogout = () => {
    localStorage.removeItem('pionex_token');
    setToken(null);
  };

  if (!token) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', backgroundColor: 'var(--bg-dark-main)' }}>
        <div className="grid-card" style={{ width: '380px', padding: '2rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', gap: '0.75rem', marginBottom: '1.5rem' }}>
            <span className="px-icon">PX</span>
            <div>
              <div className="brand-title">Pionex Control</div>
              <div className="brand-subtitle">standalone / production</div>
            </div>
          </div>

          {loginError && (
            <div style={{ backgroundColor: '#2a1215', color: '#f87171', border: '1px solid #991b1b', padding: '0.75rem', borderRadius: '0.5rem', marginBottom: '1rem', fontSize: '0.85rem', textAlign: 'center' }}>
              {loginError}
            </div>
          )}

          <form onSubmit={handleLogin}>
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-muted)', marginBottom: '0.375rem' }}>Логин</label>
              <input
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="admin"
                required
              />
            </div>

            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', fontSize: '0.8rem', color: 'var(--text-muted)', marginBottom: '0.375rem' }}>Пароль</label>
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
              disabled={loading}
              className="btn-launch"
              style={{ width: '100%', padding: '0.75rem' }}
            >
              {loading ? 'Вход...' : 'Войти в систему'}
            </button>
          </form>
        </div>
      </div>
    );
  }

  const sidebarLinks: { id: Tab; label: string; icon: string }[] = [
    { id: 'dashboard', label: 'Обзор', icon: '⚡' },
    { id: 'autogrid', label: 'Автопилот', icon: '⚡' },
    { id: 'accounts', label: 'Pionex API', icon: '❖' },
    { id: 'gridbots', label: 'Grid-боты', icon: '■' },
    { id: 'risk', label: 'Риск', icon: '◇' },
    { id: 'audit', label: 'Аудит', icon: '≡' },
    { id: 'settings', label: 'Настройки', icon: '⚙' },
  ];

  return (
    <div style={{ display: 'flex', minHeight: '100vh', backgroundColor: 'var(--bg-dark-main)' }}>
      {/* Left Sidebar */}
      <aside>
        <div>
          {/* Logo */}
          <div className="brand-logo">
            <span className="px-icon">PX</span>
            <div>
              <div className="brand-title">Pionex Control</div>
              <div className="brand-subtitle">standalone / production</div>
            </div>
          </div>

          {/* Nav Items */}
          <nav className="sidebar-nav">
            {sidebarLinks.map((item) => (
              <button
                key={item.id}
                onClick={() => setActiveTab(item.id)}
                className={`sidebar-link ${activeTab === item.id ? 'active' : ''}`}
              >
                <span>{item.icon}</span>
                <span>{item.label}</span>
              </button>
            ))}
          </nav>
        </div>

        {/* Bottom User Status */}
        <div style={{ borderTop: '1px solid var(--border-card)', paddingTop: '1rem' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.375rem' }}>
            <span style={{ width: '8px', height: '8px', borderRadius: '50%', backgroundColor: '#34d399' }}></span>
            <span style={{ fontSize: '0.75rem', color: '#34d399', fontWeight: 600 }}>Connected</span>
          </div>
          <div style={{ fontSize: '0.9rem', fontWeight: 700, color: 'var(--text-bright)' }}>Codex UI Test</div>
          <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginBottom: '0.75rem' }}>codex-ui-test · ADMIN</div>
          <button
            onClick={handleLogout}
            style={{
              width: '100%',
              padding: '0.5rem',
              borderRadius: '0.5rem',
              border: '1px solid var(--border-input)',
              backgroundColor: 'transparent',
              color: 'var(--text-bright)',
              fontSize: '0.85rem',
              fontWeight: 600,
              cursor: 'pointer',
            }}
          >
            Выйти
          </button>
        </div>
      </aside>

      {/* Main Content Area */}
      <main style={{ flex: 1, padding: '2rem 2.5rem', overflowY: 'auto' }}>
        {/* Top Header */}
        <div className="top-header">
          <div>
            <h1 className="page-title">
              {activeTab === 'autogrid' ? 'Автопилот' : activeTab.toUpperCase()}
            </h1>
            <p className="page-subtitle">
              Pionex-only control plane · конфигурация и credentials из PostgreSQL
            </p>
          </div>

          <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
            <span className="badge badge-killswitch">KILL SWITCH OFF</span>
            <button
              onClick={() => window.location.reload()}
              style={{
                padding: '0.5rem 1.25rem',
                borderRadius: '0.5rem',
                backgroundColor: 'var(--bg-input)',
                border: '1px solid var(--border-input)',
                color: 'var(--text-bright)',
                fontSize: '0.875rem',
                fontWeight: 600,
                cursor: 'pointer',
              }}
            >
              Обновить
            </button>
          </div>
        </div>

        {/* Alert Banner */}
        {!alertDismissed && (
          <div className="alert-banner">
            <span>Команда autogrid.stop принята: QUEUED. Worker подтвердит итог в PostgreSQL.</span>
            <button
              onClick={() => setAlertDismissed(true)}
              style={{ background: 'none', border: 'none', color: '#34d399', cursor: 'pointer', fontSize: '1.1rem' }}
            >
              ✕
            </button>
          </div>
        )}

        {/* Render Tab Contents */}
        {activeTab === 'autogrid' && <AutoGridAutopilot canOperate={true} />}
        {activeTab === 'accounts' && <PionexAccounts canManage={true} />}
        {activeTab === 'risk' && <SettingsView token={token} />}
        {activeTab === 'audit' && <AuditLogs token={token} />}
        {activeTab === 'settings' && <MCPServer token={token} />}
        {activeTab === 'telegram' && <TelegramNotifications token={token} />}
      </main>
    </div>
  );
}
