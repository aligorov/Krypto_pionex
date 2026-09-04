-- v2.0.78 CRIT-1(c): reopen refused final-profit settles.
-- The v2.0.75 honesty gate consulted ONLY the exchange reasonBy; a manage
-- stop submitted through native cancel comes back as "user cancel" (operator
-- class), so loss-class closings kept their positive grid_funding_residual
-- figures (prod FARTCOIN +2.349 on an ANTI_HUNT loss; XMR/JTO/ICP/SUI same
-- shape). v2.0.78 gates on BOTH the stored closed_reason and the exchange
-- reason, and the finished-list decoder now reads the documented 'results'
-- key — dropping the refusal marker re-arms the 48h backfill so those rows
-- settle at their honest finals (or NULL) exactly once more.
UPDATE grid_bots
SET model_state = model_state - 'finalProfitSource',
    updated_at = NOW()
WHERE model_state->>'finalProfitSource' LIKE 'refused_%';
