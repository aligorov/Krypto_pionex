# PIONEX AI ADVISORY BOUNDARY

## Rules & Isolation
1. **Spot AI Isolation**: Pionex Spot Grid AI strategy endpoints are SPOT-only. AI price ranges (high/low) are never applied verbatim to Futures Grid bots: the deployed range always comes from the scanner's support/resistance + volatility model, and the AI-adapted proposal (AI width, PERP-centered, ±12.5% clamp) is operator-confirmable only through manual deploy.
2. **Grid Count Adoption (operator-directed exception)**: the number of grid levels is adopted from the native AI Kit `gridCount` when an account is configured (clamped to 2–500, fallback 20). It is a spacing parameter validated natively by `futuresGrid/checkParams` before any REAL create, and every adoption is recorded in `model_assumptions.gridCountSource = pionex_ai_kit` plus the audit trail.
3. **Read-Only Advisory**: the AI Advisor cannot place orders or sign requests directly.
4. **Deterministic Verification**: all AI-informed proposals must pass schema validation, Risk Engine pre-flight checks, native checkParams, and explicit user approval before execution.
