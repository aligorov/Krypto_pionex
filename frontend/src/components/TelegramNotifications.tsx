import { useState } from 'react';

interface OutboxItem {
  id: string;
  eventType: string;
  severity: 'CRITICAL' | 'WARNING' | 'INFO' | 'SUCCESS';
  payload: string;
  status: 'PENDING' | 'SENT' | 'FAILED';
  attempts: number;
  createdAt: string;
}

export default function TelegramNotifications({ token: _token }: { token: string }) {
  const [botToken, setBotToken] = useState('');
  const [chatID, setChatID] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [testMessage, setTestMessage] = useState<string | null>(null);

  const [outbox] = useState<OutboxItem[]>([
    {
      id: 'out-101',
      eventType: 'GRID_BOT_CREATED',
      severity: 'SUCCESS',
      payload: 'Native Futures Grid Bot GRID_987654321 created for BTC_USDT_PERP',
      status: 'SENT',
      attempts: 1,
      createdAt: new Date().toISOString(),
    },
    {
      id: 'out-102',
      eventType: 'RISK_KILL_SWITCH_ACTIVE',
      severity: 'CRITICAL',
      payload: 'Pre-flight check rejected entry order: Kill switch enabled in PostgreSQL',
      status: 'SENT',
      attempts: 1,
      createdAt: new Date(Date.now() - 3600000).toISOString(),
    },
  ]);

  const handleTestNotification = () => {
    setTestMessage('Test alert successfully queued to Telegram Outbox!');
    setTimeout(() => setTestMessage(null), 4000);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Bot Configuration Card */}
      <div className="grid-card">
        <h3>Telegram Notification Settings</h3>
        {testMessage && (
          <div style={{ backgroundColor: 'rgba(34, 197, 94, 0.2)', color: '#4ade80', padding: '0.75rem', borderRadius: '0.375rem', marginBottom: '1rem' }}>
            {testMessage}
          </div>
        )}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: '1rem' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Telegram Bot Token</label>
            <input
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder="123456789:ABCdefGhIJKlmNoPQRsTUVwxyZ"
              style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
            />
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.875rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Telegram Chat ID</label>
            <input
              type="text"
              value={chatID}
              onChange={(e) => setChatID(e.target.value)}
              placeholder="-1001234567890"
              style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', backgroundColor: '#0f172a', color: '#fff', border: '1px solid #334155', boxSizing: 'border-box' }}
            />
          </div>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: '1rem', marginTop: '1.25rem' }}>
          <button
            onClick={handleTestNotification}
            style={{ padding: '0.625rem 1.25rem', backgroundColor: 'var(--accent-color)', color: '#0f172a', border: 'none', borderRadius: '0.375rem', fontWeight: 'bold', cursor: 'pointer' }}
          >
            Send Test Notification
          </button>
          <label style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', color: 'var(--text-muted)', cursor: 'pointer' }}>
            <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            Enable Outbox Dispatcher
          </label>
        </div>
      </div>

      {/* Transactional Outbox Table */}
      <div className="grid-card">
        <h3>Transactional Outbox History</h3>
        <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '1rem', textAlign: 'left' }}>
          <thead>
            <tr style={{ borderBottom: '1px solid #334155', color: 'var(--text-muted)' }}>
              <th style={{ padding: '0.5rem' }}>Event Type</th>
              <th style={{ padding: '0.5rem' }}>Severity</th>
              <th style={{ padding: '0.5rem' }}>Payload Message</th>
              <th style={{ padding: '0.5rem' }}>Status</th>
              <th style={{ padding: '0.5rem' }}>Attempts</th>
              <th style={{ padding: '0.5rem' }}>Created At</th>
            </tr>
          </thead>
          <tbody>
            {outbox.map((item) => (
              <tr key={item.id} style={{ borderBottom: '1px solid #1e293b' }}>
                <td style={{ padding: '0.75rem 0.5rem', fontFamily: 'monospace' }}>{item.eventType}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className={`badge ${item.severity === 'CRITICAL' ? 'badge-danger' : item.severity === 'SUCCESS' ? 'badge-success' : 'badge-warning'}`}>
                    {item.severity}
                  </span>
                </td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{item.payload}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className="badge badge-success">{item.status}</span>
                </td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{item.attempts}</td>
                <td style={{ padding: '0.75rem 0.5rem', color: 'var(--text-muted)' }}>{new Date(item.createdAt).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
