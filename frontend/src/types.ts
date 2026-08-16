export type Role = 'VIEWER' | 'OPERATOR' | 'ADMIN';

export interface User {
  id: string;
  username: string;
  displayName: string;
  email: string | null;
  role: Role;
  isActive: boolean;
  mustChangePassword: boolean;
  twoFactorEnabled: boolean;
  lockedUntil: string | null;
  lastLoginAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface TOTPSetupResponse {
  secret: string;
  otpauthUrl: string;
  recoveryCodes: string[];
}

export interface LoginResponse {
  user?: User;
  sessionToken?: string;
  csrfToken?: string;
  expiresAt?: string;
  requires2fa?: boolean;
}

export interface IPBan {
  ip: string;
  failedAttempts: number;
  firstFailedAt: string;
  lastFailedAt: string;
  bannedUntil: string | null;
  reason: string;
  createdAt: string;
}

export interface WhitelistEntry {
  id: number;
  ipOrCidr: string;
  description: string;
  createdBy: string;
  createdAt: string;
}

export interface MyIPResponse {
  ip: string;
  whitelisted: boolean;
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
  pnlTargetMode: 'DYNAMIC' | 'FIXED';
  pnlTargetUsdt: string;
  maxLossUsdt: string;
  manageIntervalSeconds: number;
  rangeBreakBufferPct: string;
  maxAdjustmentsPerBot: number;
  aiKitEnabled: boolean;
  aiAutotuneEnabled: boolean;
  aiAutotuneIntervalSeconds: number;
  lastAutotuneAt: string | null;
  lastAutotuneNotes: string | null;
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
  leverageReason?: string;
  leverageMode?: string;
  baseLeverage?: number;
  quoteInvestment: string;
  realizedPnlUsdt: string | null;
  unrealizedPnlUsdt: string | null;
  reconciliationState: string;
  adjustmentsCount: number;
  pnlTargetUsdt: string | null;
  maxLossUsdt: string | null;
  updatedAt: string;
}

export interface AutoGridClosedBot {
  id: string;
  source: 'PAPER' | 'REAL';
  symbol: string;
  direction: string;
  quoteInvestment: string;
  realizedPnlUsdt: string | null;
  closedReason: string | null;
  status: string;
  closedAt: string | null;
}

export interface AutoGridPnLSummary {
  realizedUsdt: string;
  unrealizedUsdt: string;
  totalUsdt: string;
  closedBots: number;
  profitable: number;
}

export interface ExchangeSnapshot {
  connected: boolean;
  accountName?: string;
  error?: string;
  coins: Array<{ coin: string; free: string; frozen: string; debts: string }>;
  usdtFree: string;
  usdtFrozen: string;
  usdtDebts: string;
  spotCoins: Array<{ coin: string; free: string; frozen: string }>;
  spotUsdtFree: string;
  spotUsdtFrozen: string;
  totalUsdt: string;
  updatedAt: string;
}

export interface AutoGridState {
  settings: AutoGridSettings;
  lastScan: AutoGridScanRun | null;
  candidates: AutoGridCandidate[];
  activeBots: AutoGridBot[];
  closedBots: AutoGridClosedBot[];
  pnl: { paper: AutoGridPnLSummary; real: AutoGridPnLSummary };
  exchange?: ExchangeSnapshot;
  metricDefinitions: Record<string, string>;
  featureAvailability: Record<string, string>;
}

export interface SpotGridAIStrategy {
  annualized: string;
  totalApr: string;
  high: string;
  low: string;
  gridCount: number;
  strategyId: string;
  volatility: string;
  maxDrawDown: string;
  options: Array<{
    period: number;
    annualized: string;
    high: string;
    low: string;
    gridCount: number;
    volatility: string;
    maxDrawDown: string;
    suitabilityMin: string;
    suitabilityMax: string;
  }>;
}

export interface AIKitResponse {
  symbol: string;
  advisory: { boundary: string };
  strategy: SpotGridAIStrategy;
  futuresAdapted: {
    lower: string;
    upper: string;
    gridCount: number;
    note: string;
  };
}

export interface AutoGridPreset {
  id: string;
  title: string;
  phase: string;
  description: string;
  whenToUse: string;
  patch: {
    maxActiveBots?: number;
    leverage?: number;
    minSharpe?: string;
    minEvPct?: string;
    stopLossMode?: string;
    adaptiveLeverageEnabled?: boolean;
    densityGridEnabled?: boolean;
    candleInterval?: string;
    lookbackCandles?: number;
    maxSymbolsPerScan?: number;
    scanIntervalSeconds?: number;
    minVolume24h?: string;
    minVolatilityPct?: string;
    maxVolatilityPct?: string;
    maxDrawdownPct?: string;
    minProfitFactor?: string;
    pnlTargetUsdt?: string;
    maxLossUsdt?: string;
    manageIntervalSeconds?: number;
    rangeBreakBufferPct?: string;
    maxAdjustmentsPerBot?: number;
    aiKitEnabled?: boolean;
  };
}

export interface LLMSettings {
  id: number;
  enabled: boolean;
  provider: 'gemini' | 'anthropic' | 'openrouter' | 'custom';
  apiKey?: string;
  apiKeyMasked: string;
  model: string;
  baseUrl: string;
  temperature: number;
  thinkingEnabled: boolean;
  requireApprovalToDeploy: boolean;
  auditIntervalSeconds: number;
  updatedAt: string;
}

export interface LLMAuditRecord {
  id: string;
  candidateId?: string;
  symbol: string;
  provider: string;
  model: string;
  decision: 'APPROVED' | 'REJECTED';
  confidence: string;
  regime: string;
  reasoning: string;
  recommendedParams?: Record<string, any>;
  latencyMs: number;
  createdAt: string;
}
