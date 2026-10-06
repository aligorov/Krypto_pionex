-- 0063 v2.0.191 (audit 05.10 F02): backfill the two state machines on the
-- 1290 already-simulated shadow rows that were completed BEFORE the state
-- columns existed in the write path — they stayed OPEN/PENDING and were
-- invisible to the gate-quality aggregates (calc_state='DONE' filter).

UPDATE shadow_candidates
SET pos_state = CASE
        WHEN outcome_reason LIKE 'TAKE_PROFIT%' OR outcome_reason LIKE 'TRAILING%' THEN 'CLOSED_TP'
        WHEN outcome_reason LIKE 'STOP_LOSS%' OR outcome_reason LIKE 'STRUCT_INVALID%'
             OR outcome_reason LIKE 'RANGE_BREAK%' THEN 'CLOSED_SL'
        WHEN outcome_reason IN ('WINDOW_END', 'WINDOW_END_AT_CLOSED_AT') THEN 'HORIZON_END'
        WHEN outcome_reason IS NULL OR outcome_reason = '' THEN 'OPEN'
        ELSE 'INVALIDATED'
    END,
    calc_state = CASE
        WHEN outcome_reason IS NULL OR outcome_reason = '' THEN 'PENDING'
        ELSE 'DONE'
    END,
    closed_at = COALESCE(closed_at, simulated_at)
WHERE simulated = TRUE
  AND pos_state = 'OPEN'
  AND calc_state = 'PENDING';
