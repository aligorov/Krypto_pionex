import { useCallback, useEffect, useState } from 'react';
import { api, ApiError } from './api';
import type { Dashboard, LoginResponse, Role, User } from './types';
import PionexAccounts from './components/PionexAccounts';
import AutoGridAutopilot from './components/AutoGridAutopilot';
import Candidates from './components/Candidates';
import Bots from './components/Bots';
import Overview from './components/Overview';
import RiskSettings from './components/RiskSettings';
import AuditLogs from './components/AuditLogs';
import MCPServer from './components/MCPServer';

type Tab = 'overview' | 'autogrid' | 'candidates' | 'bots' | 'accounts' | 'risk' | 'audit' | 'mcp';

const canOperate = (role: Role): boolean => role === 'OPERATOR' || role === 'ADMIN';
const canManage = (role: Role): boolean => role === 'ADMIN';

export default function App() {
  const [user, setUser] = useState<User | null>(null);
  const [booting, setBooting] = useState(true);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [loginError, setLoginError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [overview, setOverview] = useState<Dashboard | null>(null);

  const [activeTab, setActiveTab] = useState<Tab>('overview');

  const loadOverview = useCallback(() => {
    api<Dashboard>('/api/dashboard')
      .then(setOverview)
      .catch(() => setOverview(null));
  }, []);

  useEffect(() => {
    let active = true;
    api<User>('/api/auth/me')
      .then((me) => {
        if (active) setUser(me);
      })
      .catch(() => {
        /* not signed in yet */
      })
      .finally(() => {
        if (active) setBooting(false);
      });
    return () => {
      active = false;
    };
  }, []);

  useEffect(() => {
    if (!user) return;
    loadOverview();
    const timer = window.setInterval(loadOverview, 15000);
    return () => window.clearInterval(timer);
  }, [user, loadOverview]);

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoginError(null);
    setLoading(true);
    try {
      const data = await api<LoginResponse>('/api/auth/login', {
        method: 'POST',
        body: JSON.stringify({ username, password }),
      });
      setUser(data.user);
      setPassword('');
    } catch (error) {
      setLoginError(error instanceof ApiError ? error.message : 'Ошибка подключения к серверу');
    } finally {
      setLoading(false);
    }
  };

  const handleLogout = async () => {
    try {
      await api('/api/auth/logout', { method: 'POST' });
    } catch {
      /* the session cookie is being dropped either way */
    }
    setUser(null);
    setOverview(null);
  };

  if (booting) {
    return <div className="splash">Загрузка Pionex Control…</div>;
  }

  if (!user) {
    return (
      <div className="login-page">
        <div className="login-hero">
          <span className="eyebrow">PIONEX-ONLY · NATIVE BOTS</span>
          <h1>Грид-автопилот на нативном API Pionex</h1>
          <p>
            Скан волатильности и режима рынка, AI Kit биржи, нативный take-profit
            на бирже и ведение каждого бота до цели PnL.
          </p>
          <ul>
            <li>Отбор пар: волатильность, ADX/EMA-режим, позиция в диапазоне</li>
            <li>Каждый бот: цель +PnL и стоп-лосс исполняются самим Pionex</li>
            <li>Пробой диапазона: сдвиг сетки adjustParams или закрытие</li>
          </ul>
        </div>
        <div className="login-card">
          <div className="brand">
            <span className="brand-mark">PX</span>
            <div>
              <div className="brand-title">Pionex Control</div>
              <div className="muted">standalone / production</div>
            </div>
          </div>

          {loginError && <div className="alert danger">{loginError}</div>}

          <form className="form-stack" onSubmit={handleLogin}>
            <label>
              Логин
              <input
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="username"
                required
              />
            </label>
            <label>
              Пароль
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                required
              />
            </label>
            <button type="submit" className="button primary" disabled={loading}>
              {loading ? 'Вход…' : 'Войти'}
            </button>
          </form>
        </div>
      </div>
    );
  }

  const sidebarLinks: { id: Tab; label: string }[] = [
    { id: 'overview', label: 'Обзор' },
    { id: 'autogrid', label: 'Автопилот' },
    { id: 'candidates', label: 'Кандидаты' },
    { id: 'bots', label: 'Боты · PnL' },
    { id: 'accounts', label: 'Pionex API' },
    { id: 'risk', label: 'Риск' },
    { id: 'audit', label: 'Аудит' },
    { id: 'mcp', label: 'MCP' },
  ];

  const killSwitchOn = overview?.killSwitchEnabled ?? false;

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div>
          <div className="brand">
            <span className="brand-mark">PX</span>
            <div>
              <div className="brand-title">Pionex Control</div>
              <div className="muted">Pionex-only · native bots</div>
            </div>
          </div>

          <nav className="nav">
            {sidebarLinks.map((item) => (
              <button
                key={item.id}
                onClick={() => setActiveTab(item.id)}
                className={`nav-item ${activeTab === item.id ? 'active' : ''}`}
              >
                <span>{item.label}</span>
              </button>
            ))}
          </nav>
        </div>

        <div className="sidebar-footer">
          <div className="identity">
            <div style={{ fontWeight: 700 }}>{user.displayName || user.username}</div>
            <div className="muted">{user.username} · {user.role}</div>
          </div>
          <button className="button" onClick={handleLogout}>Выйти</button>
        </div>
      </aside>

      <main>
        <div className="topbar">
          <div>
            <h1 className="page-title">
              {sidebarLinks.find((item) => item.id === activeTab)?.label}
            </h1>
            <p className="muted">
              Конфигурация и credentials из PostgreSQL · нативные грид-боты Pionex
            </p>
          </div>
          <div className="topbar-actions">
            <span className={`badge ${killSwitchOn ? 'badge-danger' : 'badge-ok'}`}>
              KILL SWITCH {killSwitchOn ? 'ON' : 'OFF'}
            </span>
            {overview && (
              <span className="badge">
                Команд в очереди: {overview.pendingCommands}
              </span>
            )}
            <button className="button" onClick={loadOverview}>Обновить</button>
          </div>
        </div>

        {user && activeTab === 'overview' && <Overview onRefresh={loadOverview} />}
        {activeTab === 'autogrid' && (
          <AutoGridAutopilot canOperate={canOperate(user.role)} accountsHref={() => setActiveTab('accounts')} />
        )}
        {activeTab === 'candidates' && <Candidates canOperate={canOperate(user.role)} />}
        {activeTab === 'bots' && <Bots canOperate={canOperate(user.role)} />}
        {activeTab === 'accounts' && <PionexAccounts canManage={canManage(user.role)} />}
        {activeTab === 'risk' && <RiskSettings canManage={canManage(user.role)} />}
        {activeTab === 'audit' && <AuditLogs />}
        {activeTab === 'mcp' && <MCPServer canManage={canManage(user.role)} />}
      </main>
    </div>
  );
}
