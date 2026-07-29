export type Role = 'VIEWER' | 'OPERATOR' | 'ADMIN';

export interface User {
  id: string;
  username: string;
  displayName: string;
  email: string | null;
  role: Role;
  isActive: boolean;
  mustChangePassword: boolean;
  lockedUntil: string | null;
  lastLoginAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface UserSettings {
  userId: string;
  language: 'ru' | 'en';
  timezone: string;
  theme: 'dark' | 'light' | 'system';
  defaultAccountId: string | null;
  preferences: Record<string, unknown>;
  updatedAt: string;
}

export interface Dashboard {
  version: string;
  gitCommit: string;
  buildTime: string;
  databaseHealthy: boolean;
  killSwitchEnabled: boolean;
  activeAccounts: number;
  runningGrids: number;
  openPatternOrders: number;
  openIncidents: number;
  pendingCommands: number;
  mcpEnabled: boolean;
  realGridEnabled: boolean;
  realPatternEnabled: boolean;
  checkedAt: string;
}

export interface RiskSettings {
  id: number;
  killSwitchEnabled: boolean;
  maxAccountExposureUsd: string;
  maxSymbolExposureUsd: string;
  maxDailyLossUsd: string;
  maxLeverage: number;
  maxActiveGridBots: number;
  maxOpenPositions: number;
}

export interface Account {
  id: string;
  name: string;
  keyFingerprint: string;
  isEnabled: boolean;
  isPaper: boolean;
  hasReadPermission: boolean;
  hasFuturesPermission: boolean;
  hasBotPermission: boolean;
  capabilityStatus: string;
  lastVerifiedAt: string | null;
  lastError: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface GridBot {
  id: string;
  accountId: string;
  symbol: string;
  buOrderId: string | null;
  status: string;
  direction: string;
  gridType: string;
  lowerPrice: string;
  upperPrice: string;
  gridNum: number;
  leverage: number;
  quoteInvestment: string;
  createdAt: string;
  updatedAt: string;
}

export interface PatternOrder {
  id: string;
  accountId: string;
  symbol: string;
  clientOrderId: string;
  pionexOrderId: string | null;
  patternType: string;
  side: string;
  orderType: string;
  price: string;
  quantity: string;
  status: string;
  createdAt: string;
  updatedAt: string;
}

export interface ConfigEntry {
  key: string;
  value: unknown;
  description: string;
  isSensitive: boolean;
  updatedBy: string | null;
  updatedAt: string;
}

export interface FeatureFlag {
  name: string;
  enabled: boolean;
  description: string;
  updatedBy: string | null;
  updatedAt: string;
}

export interface LogEntry {
  id: number;
  occurredAt: string;
  level: string;
  component: string;
  message: string;
  requestId: string;
  actorId: string | null;
  accountId: string | null;
  symbol: string;
  aggregateId: string;
  fields: Record<string, unknown>;
}

export interface AuditEvent {
  action: string;
  actor: string;
  actorId: string | null;
  actorType: string;
  resourceType: string;
  resourceId: string;
  outcome: string;
  requestId: string;
  ipAddress: string;
  userAgent: string;
  details: Record<string, unknown>;
  createdAt: string;
}

export interface APIToken {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  expiresAt: string | null;
  revokedAt: string | null;
  lastUsedAt: string | null;
  createdAt: string;
}

export interface ControlCommand {
  id: string;
  actorId: string | null;
  actorType: string;
  commandType: string;
  resourceType: string;
  resourceId: string;
  sanitizedArguments: Record<string, unknown>;
  idempotencyKey: string;
  status: string;
  confirmationExpiresAt: string | null;
  riskResult: Record<string, unknown>;
  result: Record<string, unknown>;
  errorMessage: string | null;
  createdAt: string;
  confirmedAt: string | null;
  executedAt: string | null;
  updatedAt: string;
}

export interface PreparedCommand {
  command: ControlCommand;
  confirmationCode?: string;
}

export type Tab =
  | 'dashboard'
  | 'accounts'
  | 'autogrid'
  | 'grids'
  | 'risk'
  | 'settings'
  | 'audit';

export interface LoginResponse {
  user: User;
  csrfToken: string;
  expiresAt: string;
}

export interface BootstrapStatus {
  initialized: boolean;
  setupCommand: string;
}

export type AutoGridStatus =
  | 'STOPPED'
  | 'STARTING'
  | 'RUNNING'
  | 'PAUSED'
  | 'EMERGENCY_STOPPED';

export interface AutoGridSettings {
  id: string;
  accountId: string | null;
  status: AutoGridStatus;
  executionMode: 'PAPER' | 'REAL';
  budgetUsdt: string;
  maxActiveBots: number;
  leverage: number;
  minSharpe: string;
  minEvPct: string;
  stopLossMode: 'NONE' | 'ADAPTIVE_ATR';
  smartPnlEnabled: boolean;
  adaptiveLeverageEnabled: boolean;
  densityGridEnabled: boolean;
  candleInterval: string;
  lookbackCandles: number;
  maxSymbolsPerScan: number;
  scanIntervalSeconds: number;
  minVolume24h: string;
  minVolatilityPct: string;
  maxVolatilityPct: string;
  maxDrawdownPct: string;
  minProfitFactor: string;
  feeBps: string;
  slippageBps: string;
  lastError: string | null;
  lastStartedAt: string | null;
  lastStoppedAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface AutoGridScanRun {
  id: string;
  status: string;
  candidatesFound: number;
  errorMessage: string | null;
  startedAt: string;
  completedAt: string | null;
}

export interface AutoGridCandidate {
  id: string;
  symbol: string;
  currentPrice: string;
  volatilityPct: string;
  volume24h: string;
  fundingRate: string | null;
  expectedValuePct: string;
  sharpe: string;
  sortino: string;
  maxDrawdownPct: string;
  winRatePct: string;
  profitFactor: string;
  turnoverProxy: string;
  score: string;
  decision: 'ACCEPTED' | 'REJECTED';
  rejectionReason: string | null;
  lowerPrice: string;
  upperPrice: string;
  gridNum: number;
  recommendedLeverage: number;
  recommendedTrend: 'long' | 'short' | 'no_trend';
  modelAssumptions: Record<string, unknown>;
  createdAt: string;
}

export interface AutoGridBot {
  id: string;
  source: 'PAPER' | 'REAL';
  accountId: string | null;
  buOrderId: string | null;
  symbol: string;
  status: string;
  direction: string;
  gridType: string;
  lowerPrice: string;
  upperPrice: string;
  gridNum: number;
  leverage: number;
  quoteInvestment: string;
  realizedPnlUsdt: string | null;
  unrealizedPnlUsdt: string | null;
  reconciliationState: string;
  updatedAt: string;
}

export interface AutoGridState {
  settings: AutoGridSettings;
  lastScan: AutoGridScanRun | null;
  candidates: AutoGridCandidate[];
  activeBots: AutoGridBot[];
  metricDefinitions: Record<string, string>;
  featureAvailability: Record<string, string>;
}
