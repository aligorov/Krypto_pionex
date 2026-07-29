# PIONEX AI ADVISORY BOUNDARY

## Rules & Isolation
1. **Spot AI Isolation**: Pionex Spot Grid AI strategy endpoints are SPOT-ONLY. It is strictly forbidden to apply Spot AI price ranges to Futures Grid bots.
2. **Read-Only Advisory**: The AI Advisor cannot place orders or sign requests directly.
3. **Deterministic Verification**: All AI proposals must pass schema validation, Risk Engine pre-flight checks, and explicit user approval before execution.
