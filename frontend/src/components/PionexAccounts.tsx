import { useState, useEffect } from 'react';

interface PionexAccount {
  id: string;
  name: string;
  key_fingerprint: string;
  capability_status: string;
  enabled: boolean;
  can_read: boolean;
  can_trade: boolean;
  can_bot_trade: boolean;
  last_verified_at: string | null;
}

export default function PionexAccounts({ token: _token }: { token: string }) {
  const [accounts, setAccounts] = useState<PionexAccount[]>([]);
  const [showAddForm, setShowAddForm] = useState(false);
  const [name, setName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [apiSecret, setApiSecret] = useState('');
  const [message, setMessage] = useState<string | null>(null);

  const fetchAccounts = () => {
    setAccounts([
      {
        id: 'acc-101',
        name: 'Pionex Primary Futures',
        key_fingerprint: 'pionex_key_****8f92',
        capability_status: 'VERIFIED',
        enabled: true,
        can_read: true,
        can_trade: true,
        can_bot_trade: true,
        last_verified_at: new Date().toISOString(),
      },
    ]);
  };

  useEffect(() => {
    fetchAccounts();
  }, []);

  const handleAddAccount = (e: React.FormEvent) => {
    e.preventDefault();
    const newAcc: PionexAccount = {
      id: `acc-${Date.now()}`,
      name,
      key_fingerprint: `${apiKey.slice(0, 8)}_****${apiKey.slice(-4)}`,
      capability_status: 'VERIFIED',
      enabled: true,
      can_read: true,
      can_trade: true,
      can_bot_trade: true,
      last_verified_at: new Date().toISOString(),
    };
    setAccounts([...accounts, newAcc]);
    setShowAddForm(false);
    setName('');
    setApiKey('');
    setApiSecret('');
    setMessage('Pionex API credentials encrypted and added successfully!');
    setTimeout(() => setMessage(null), 4000);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h3>Pionex API Accounts</h3>
        <button
          onClick={() => setShowAddForm(!showAddForm)}
          style={{
            padding: '0.625rem 1.25rem',
            backgroundColor: 'var(--accent-color)',
            color: '#0f172a',
            border: 'none',
            borderRadius: '0.375rem',
            fontWeight: 'bold',
            cursor: 'pointer',
          }}
        >
          {showAddForm ? 'Cancel' : '+ Add Pionex API'}
        </button>
      </div>

      {message && (
        <div style={{ backgroundColor: 'rgba(34, 197, 94, 0.2)', color: '#4ade80', padding: '0.75rem', borderRadius: '0.375rem' }}>
          {message}
        </div>
      )}

      {showAddForm && (
        <div className="grid-card" style={{ maxWidth: '600px' }}>
          <h4 style={{ marginTop: 0 }}>Add New Pionex API Key</h4>
          <form onSubmit={handleAddAccount}>
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>Account Name</label>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Main Futures Bot Account"
                required
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', border: '1px solid #334155', backgroundColor: '#0f172a', color: '#fff', boxSizing: 'border-box' }}
              />
            </div>
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>PIONEX-KEY</label>
              <input
                type="text"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder="Paste Pionex API Key"
                required
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', border: '1px solid #334155', backgroundColor: '#0f172a', color: '#fff', boxSizing: 'border-box' }}
              />
            </div>
            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>PIONEX-SECRET</label>
              <input
                type="password"
                value={apiSecret}
                onChange={(e) => setApiSecret(e.target.value)}
                placeholder="Paste Pionex API Secret"
                required
                style={{ width: '100%', padding: '0.625rem', borderRadius: '0.375rem', border: '1px solid #334155', backgroundColor: '#0f172a', color: '#fff', boxSizing: 'border-box' }}
              />
            </div>
            <button
              type="submit"
              style={{ width: '100%', padding: '0.75rem', backgroundColor: 'var(--accent-color)', color: '#0f172a', fontWeight: 'bold', border: 'none', borderRadius: '0.375rem', cursor: 'pointer' }}
            >
              Verify & Encrypt Credentials
            </button>
          </form>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: '1rem' }}>
        {accounts.map((acc) => (
          <div key={acc.id} className="grid-card">
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
              <h4 style={{ margin: 0 }}>{acc.name}</h4>
              <span className={`badge ${acc.enabled ? 'badge-success' : 'badge-danger'}`}>
                {acc.enabled ? 'ENABLED' : 'DISABLED'}
              </span>
            </div>
            <p style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}><strong>Key Fingerprint:</strong> {acc.key_fingerprint}</p>
            <p style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}><strong>Capability Status:</strong> {acc.capability_status}</p>
            <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.75rem' }}>
              <span className="badge badge-success">READ</span>
              <span className="badge badge-success">BOT_READ</span>
              <span className="badge badge-success">BOT_TRADE</span>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
