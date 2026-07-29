# NATIVE FUTURES GRID BOT LIFECYCLE

## State Machine Diagram
```
[ DRAFT ] ---> [ PENDING_SUBMISSION ] ---> [ SUBMITTED ] ---> [ RUNNING ]
   |                    |                      |                    |
   v                    v                      v                    v
[REJECTED]        [SUBMISSION_UNKNOWN]     [STOPPING]           [STOPPING]
                        |                      |                    |
                        v                      v                    v
                 [RECONCILIATION]          [STOPPED]            [STOPPED]
```

## State Rules
1. **DRAFT**: Parameters validated locally.
2. **PENDING_SUBMISSION**: Entry recorded in PostgreSQL `grid_bots` table.
3. **SUBMITTED**: POST request sent to `/api/v1/bot/futuresGrid/create`.
4. **RUNNING**: Transitions ONLY when valid remote `buOrderId` is received and confirmed via GET request.
5. **STOPPING / STOPPED**: Closed via `/api/v1/bot/futuresGrid/cancel` and residual positions verified flat.
