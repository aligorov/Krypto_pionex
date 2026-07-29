import { useState } from 'react';

interface AuditEvent {
  id: string;
  action: string;
  actor: string;
  resourceType: string;
  outcome: 'SUCCESS' | 'DENIED' | 'FAILED';
  ipAddress: string;
  timestamp: string;
}

interface AppLog {
  id: string;
  level: 'INFO' | 'WARN' | 'ERROR' | 'DEBUG';
  component: string;
  message: string;
  timestamp: string;
}

export default function AuditLogs({ token: _token }: { token: string }) {
  const [activeSubTab, setActiveSubTab] = useState<'audit' | 'logs'>('audit');
  const [levelFilter, setLevelFilter] = useState<'ALL' | 'INFO' | 'WARN' | 'ERROR'>('ALL');

  const [auditEvents] = useState<AuditEvent[]>([
    {
      id: 'aud-1',
      action: 'USER_LOGIN',
      actor: 'admin',
      resourceType: 'session',
      outcome: 'SUCCESS',
      ipAddress: '127.0.0.1',
      timestamp: new Date().toISOString(),
    },
    {
      id: 'aud-2',
      action: 'PIONEX_API_KEY_ADD',
      actor: 'admin',
      resourceType: 'pionex_account',
      outcome: 'SUCCESS',
      ipAddress: '127.0.0.1',
      timestamp: new Date(Date.now() - 1800000).toISOString(),
    },
    {
      id: 'aud-3',
      action: 'GRID_BOT_CREATE_INTENT',
      actor: 'system',
      resourceType: 'grid_bot',
      outcome: 'SUCCESS',
      ipAddress: 'internal',
      timestamp: new Date(Date.now() - 3600000).toISOString(),
    },
  ]);

  const [logs] = useState<AppLog[]>([
    {
      id: 'log-1',
      level: 'INFO',
      component: 'pionex_signer',
      message: 'HMAC-SHA256 signature generated cleanly for /api/v1/bot/orders/futuresGrid/create',
      timestamp: new Date().toISOString(),
    },
    {
      id: 'log-2',
      level: 'INFO',
      component: 'websocket',
      message: 'Connected to Pionex Private Stream wss://ws.pionex.com/wsUA',
      timestamp: new Date(Date.now() - 60000).toISOString(),
    },
    {
      id: 'log-3',
      level: 'WARN',
      component: 'risk_engine',
      message: 'Exposure limit evaluation: current exposure $300.00 / max $1000.00',
      timestamp: new Date(Date.now() - 120000).toISOString(),
    },
    {
      id: 'log-4',
      level: 'ERROR',
      component: 'rate_limiter',
      message: 'HTTP 429 received from Pionex REST API - rate limiter cooldown 60s active',
      timestamp: new Date(Date.now() - 300000).toISOString(),
    },
  ]);

  const filteredLogs = logs.filter((log) => levelFilter === 'ALL' || log.level === levelFilter);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Subnav */}
      <div style={{ display: 'flex', gap: '1rem', borderBottom: '1px solid #334155', paddingBottom: '0.5rem' }}>
        <button
          className={`nav-btn ${activeSubTab === 'audit' ? 'active' : ''}`}
          onClick={() => setActiveSubTab('audit')}
        >
          Audit Trail Events
        </button>
        <button
          className={`nav-btn ${activeSubTab === 'logs' ? 'active' : ''}`}
          onClick={() => setActiveSubTab('logs')}
        >
          Application System Logs
        </button>
      </div>

      {activeSubTab === 'audit' && (
        <div className="grid-card">
          <h3>Security Audit Trail (`audit_events`)</h3>
          <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '1rem', textAlign: 'left' }}>
            <thead>
              <tr style={{ borderBottom: '1px solid #334155', color: 'var(--text-muted)' }}>
                <th style={{ padding: '0.5rem' }}>Action</th>
                <th style={{ padding: '0.5rem' }}>Actor</th>
                <th style={{ padding: '0.5rem' }}>Resource Type</th>
                <th style={{ padding: '0.5rem' }}>Outcome</th>
                <th style={{ padding: '0.5rem' }}>IP Address</th>
                <th style={{ padding: '0.5rem' }}>Timestamp</th>
              </tr>
            </thead>
            <tbody>
              {auditEvents.map((evt) => (
                <tr key={evt.id} style={{ borderBottom: '1px solid #1e293b' }}>
                  <td style={{ padding: '0.75rem 0.5rem', fontFamily: 'monospace', fontWeight: 'bold' }}>{evt.action}</td>
                  <td style={{ padding: '0.75rem 0.5rem' }}>{evt.actor}</td>
                  <td style={{ padding: '0.75rem 0.5rem' }}>{evt.resourceType}</td>
                  <td style={{ padding: '0.75rem 0.5rem' }}>
                    <span className={`badge ${evt.outcome === 'SUCCESS' ? 'badge-success' : 'badge-danger'}`}>
                      {evt.outcome}
                    </span>
                  </td>
                  <td style={{ padding: '0.75rem 0.5rem', color: 'var(--text-muted)' }}>{evt.ipAddress}</td>
                  <td style={{ padding: '0.75rem 0.5rem', color: 'var(--text-muted)' }}>{new Date(evt.timestamp).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {activeSubTab === 'logs' && (
        <div className="grid-card">
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
            <h3 style={{ margin: 0 }}>System Application Logs</h3>
            <div style={{ display: 'flex', gap: '0.5rem' }}>
              {(['ALL', 'INFO', 'WARN', 'ERROR'] as const).map((lvl) => (
                <button
                  key={lvl}
                  className={`nav-btn ${levelFilter === lvl ? 'active' : ''}`}
                  onClick={() => setLevelFilter(lvl)}
                >
                  {lvl}
                </button>
              ))}
            </div>
          </div>

          <div style={{ backgroundColor: '#090d16', padding: '1rem', borderRadius: '0.5rem', fontFamily: 'monospace', fontSize: '0.85rem' }}>
            {filteredLogs.map((log) => (
              <div key={log.id} style={{ marginBottom: '0.5rem', display: 'flex', gap: '1rem' }}>
                <span style={{ color: 'var(--text-muted)' }}>[{new Date(log.timestamp).toLocaleTimeString()}]</span>
                <span style={{ color: log.level === 'ERROR' ? '#f87171' : log.level === 'WARN' ? '#fbbf24' : '#38bdf8', fontWeight: 'bold', width: '60px' }}>
                  {log.level}
                </span>
                <span style={{ color: '#a7f3d0' }}>[{log.component}]</span>
                <span>{log.message}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
