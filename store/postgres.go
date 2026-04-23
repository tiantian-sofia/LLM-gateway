package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
	"github.com/tiantian-sofia/LLM-gateway/proxy"
)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS cost_records (
    id            SERIAL PRIMARY KEY,
    recorded_at   TIMESTAMPTZ NOT NULL,
    model         TEXT NOT NULL,
    source_ip     TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    total_tokens  INTEGER NOT NULL,
    input_cost    DOUBLE PRECISION NOT NULL,
    output_cost   DOUBLE PRECISION NOT NULL,
    total_cost    DOUBLE PRECISION NOT NULL
)`

// PostgresStore implements CostStore using PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore opens a connection to PostgreSQL, verifies it with a ping,
// and creates the cost_records table if it does not exist.
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	if _, err := db.Exec(createTableSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating cost_records table: %w", err)
	}

	// Create indexes for search performance.
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_cost_recorded_at ON cost_records (recorded_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cost_model ON cost_records (model)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			db.Close()
			return nil, fmt.Errorf("creating index: %w", err)
		}
	}

	return &PostgresStore{db: db}, nil
}

// Insert persists a single cost record to the database.
func (s *PostgresStore) Insert(rec proxy.CostRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO cost_records (recorded_at, model, source_ip, user_agent, input_tokens, output_tokens, total_tokens, input_cost, output_cost, total_cost)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.Time, rec.Model, rec.SourceIP, rec.UserAgent,
		rec.InputTokens, rec.OutputTokens, rec.TotalTokens,
		rec.InputCost, rec.OutputCost, rec.TotalCost,
	)
	return err
}

// Search returns cost records matching the given filter criteria.
func (s *PostgresStore) Search(filter proxy.CostFilter) ([]proxy.CostRecord, error) {
	query := `SELECT recorded_at, model, source_ip, user_agent, input_tokens, output_tokens, total_tokens, input_cost, output_cost, total_cost
		 FROM cost_records`

	var conditions []string
	var args []interface{}
	argN := 1

	if filter.StartTime != nil {
		conditions = append(conditions, fmt.Sprintf("recorded_at >= $%d", argN))
		args = append(args, *filter.StartTime)
		argN++
	}
	if filter.EndTime != nil {
		conditions = append(conditions, fmt.Sprintf("recorded_at <= $%d", argN))
		args = append(args, *filter.EndTime)
		argN++
	}
	if filter.Model != "" {
		conditions = append(conditions, fmt.Sprintf("LOWER(model) LIKE $%d", argN))
		args = append(args, "%"+strings.ToLower(filter.Model)+"%")
		argN++
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY recorded_at ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []proxy.CostRecord
	for rows.Next() {
		var rec proxy.CostRecord
		if err := rows.Scan(
			&rec.Time, &rec.Model, &rec.SourceIP, &rec.UserAgent,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
			&rec.InputCost, &rec.OutputCost, &rec.TotalCost,
		); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// List returns all cost records ordered by time ascending.
func (s *PostgresStore) List() ([]proxy.CostRecord, error) {
	rows, err := s.db.Query(
		`SELECT recorded_at, model, source_ip, user_agent, input_tokens, output_tokens, total_tokens, input_cost, output_cost, total_cost
		 FROM cost_records ORDER BY recorded_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []proxy.CostRecord
	for rows.Next() {
		var rec proxy.CostRecord
		if err := rows.Scan(
			&rec.Time, &rec.Model, &rec.SourceIP, &rec.UserAgent,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
			&rec.InputCost, &rec.OutputCost, &rec.TotalCost,
		); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// Close closes the database connection pool.
func (s *PostgresStore) Close() error {
	return s.db.Close()
}
