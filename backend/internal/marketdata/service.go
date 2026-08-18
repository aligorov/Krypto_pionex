package marketdata

import "github.com/jackc/pgx/v5/pgxpool"

// Service exposes read-only market intelligence queries over the data
// gathered by the Collector (funding, open interest, sentiment, economic
// events). It is intentionally separate from the Collector so consumers
// such as the Smart Grid Engine never depend on the write path.
type Service struct {
	db *pgxpool.Pool
}

// NewService builds a marketdata query service on top of a pgx connection pool.
func NewService(db *pgxpool.Pool) *Service {
	return &Service{db: db}
}
