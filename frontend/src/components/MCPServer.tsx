import { useState } from 'react';

interface MCPTool {
  name: string;
  description: string;
  requiredRole: 'VIEWER' | 'OPERATOR' | 'ADMIN';
  enabled: boolean;
}

export default function MCPServer({ token: _token }: { token: string }) {
  const [mcpEnabled] = useState(true);

  const [tools] = useState<MCPTool[]>([
    {
      name: 'pionex_risk_check',
      description: 'Evaluates pre-flight risk limits against PostgreSQL risk_settings table',
      requiredRole: 'OPERATOR',
      enabled: true,
    },
    {
      name: 'pionex_grid_create',
      description: 'Submits a native Futures Grid Bot request to Pionex API',
      requiredRole: 'ADMIN',
      enabled: true,
    },
    {
      name: 'pionex_account_validate',
      description: 'Verifies Pionex API Key read/trade capabilities via test REST call',
      requiredRole: 'ADMIN',
      enabled: true,
    },
  ]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* MCP Status Banner */}
      <div className="grid-card" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <div>
          <h3 style={{ margin: 0 }}>Model Context Protocol (MCP) Control Plane</h3>
          <p style={{ margin: '0.25rem 0 0 0', color: 'var(--text-muted)', fontSize: '0.875rem' }}>
            Serves AI agents and automated tools over stdio/SSE protocol
          </p>
        </div>
        <span className={`badge ${mcpEnabled ? 'badge-success' : 'badge-danger'}`}>
          {mcpEnabled ? 'MCP SERVER: ONLINE' : 'MCP SERVER: OFFLINE'}
        </span>
      </div>

      {/* Tools Registry */}
      <div className="grid-card">
        <h3>Registered MCP Tools</h3>
        <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '1rem', textAlign: 'left' }}>
          <thead>
            <tr style={{ borderBottom: '1px solid #334155', color: 'var(--text-muted)' }}>
              <th style={{ padding: '0.5rem' }}>Tool Name</th>
              <th style={{ padding: '0.5rem' }}>Description</th>
              <th style={{ padding: '0.5rem' }}>Required Role</th>
              <th style={{ padding: '0.5rem' }}>Status</th>
            </tr>
          </thead>
          <tbody>
            {tools.map((t) => (
              <tr key={t.name} style={{ borderBottom: '1px solid #1e293b' }}>
                <td style={{ padding: '0.75rem 0.5rem', fontFamily: 'monospace', fontWeight: 'bold', color: 'var(--accent-color)' }}>{t.name}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>{t.description}</td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className="badge badge-warning">{t.requiredRole}</span>
                </td>
                <td style={{ padding: '0.75rem 0.5rem' }}>
                  <span className={`badge ${t.enabled ? 'badge-success' : 'badge-danger'}`}>
                    {t.enabled ? 'ENABLED' : 'DISABLED'}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
