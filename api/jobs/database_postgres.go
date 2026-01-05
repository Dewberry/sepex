package jobs

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type PostgresDB struct {
	Handle *sql.DB
}

// Initialize the database.
func NewPostgresDB(dbConnString string) (*PostgresDB, error) {
	h, err := sql.Open("postgres", dbConnString)

	if err != nil {
		return nil, fmt.Errorf("could not connect to database. Error: %s", err.Error())
	}

	if h == nil {
		return nil, fmt.Errorf("db nil")
	}

	db := PostgresDB{Handle: h}
	err = db.createTables()
	if err != nil {
		return nil, err
	}
	return &db, nil
}

func (db *PostgresDB) updateJobHost(jid, host, hostJobID string) error {
	query := `UPDATE jobs SET host = $2, host_job_id = $3 WHERE id = $1`
	_, err := db.Handle.Exec(query, jid, host, hostJobID)
	return err
}

// createTables in the database if they do not exist already for PostgreSQL
func (postgresDB *PostgresDB) createTables() error {

	queryJobs := `
    CREATE TABLE IF NOT EXISTS jobs (
        id TEXT PRIMARY KEY,
        status TEXT NOT NULL,
        updated TIMESTAMP WITHOUT TIME ZONE NOT NULL,
        mode TEXT NOT NULL,
        host TEXT NOT NULL,
				host_job_id TEXT NOT NULL,
        process_id TEXT NOT NULL,
        submitter TEXT NOT NULL DEFAULT ''
    );

    CREATE INDEX IF NOT EXISTS idx_jobs_updated ON jobs(updated);
    CREATE INDEX IF NOT EXISTS idx_jobs_process_id ON jobs(process_id);
    CREATE INDEX IF NOT EXISTS idx_jobs_submitter ON jobs(submitter);
    `

	_, err := postgresDB.Handle.Exec(queryJobs)
	if err != nil {
		return fmt.Errorf("error creating tables: %s", err)
	}
	return nil
}

// AddJob adds a new job to the database
func (db *PostgresDB) addJob(jid, status, mode, host, hostJobID, processID, submitter string, updated time.Time) error {
	query := `INSERT INTO jobs (id, status, updated, mode, host, host_job_id, process_id, submitter) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := db.Handle.Exec(query, jid, status, updated, mode, host, hostJobID, processID, submitter)
	return err
}

// UpdateJobRecord updates a job record
func (db *PostgresDB) updateJobRecord(jid, status string, now time.Time) error {
	query := `UPDATE jobs SET status = $2, updated = $3 WHERE id = $1`
	_, err := db.Handle.Exec(query, jid, status, now)
	return err
}

// GetJob retrieves a job record by id
func (db *PostgresDB) GetJob(jid string) (JobRecord, bool, error) {
	query := `SELECT id, status, updated, mode, host, host_job_id, process_id, submitter FROM jobs WHERE id = $1`

	var jr JobRecord
	err := db.Handle.QueryRow(query, jid).Scan(
		&jr.JobID,
		&jr.Status,
		&jr.LastUpdate,
		&jr.Mode,
		&jr.Host,
		&jr.HostJobID,
		&jr.ProcessID,
		&jr.Submitter,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return JobRecord{}, false, nil
		}
		return JobRecord{}, false, err
	}
	return jr, true, nil
}

// CheckJobExist checks if a job exists in the database
func (db *PostgresDB) CheckJobExist(jid string) (bool, error) {
	query := `SELECT 1 FROM jobs WHERE id = $1`
	var exists int
	err := db.Handle.QueryRow(query, jid).Scan(&exists)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Assumes query parameters are valid
func (pgDB *PostgresDB) GetJobs(limit, offset int, processIDs, statuses, submitters []string) ([]JobRecord, error) {
	baseQuery := `SELECT id, status, updated, process_id, submitter FROM jobs`
	whereClauses := []string{}
	args := []interface{}{}

	argIndex := 1 // Start from 1 for PostgreSQL placeholders

	if len(processIDs) > 0 {
		placeholders := make([]string, len(processIDs))
		for i := range processIDs {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			argIndex++
		}
		whereClauses = append(whereClauses, "process_id IN ("+strings.Join(placeholders, ", ")+")")
		for _, pid := range processIDs {
			args = append(args, pid)
		}
	}

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i := range statuses {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			argIndex++
		}
		whereClauses = append(whereClauses, "status IN ("+strings.Join(placeholders, ", ")+")")
		for _, st := range statuses {
			args = append(args, st)
		}
	}

	if len(submitters) > 0 {
		placeholders := make([]string, len(submitters))
		for i := range submitters {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			argIndex++
		}
		whereClauses = append(whereClauses, "submitter IN ("+strings.Join(placeholders, ", ")+")")
		for _, sb := range submitters {
			args = append(args, sb)
		}
	}

	if len(whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Add limit and offset to the query and args
	query := baseQuery + fmt.Sprintf(" ORDER BY updated DESC LIMIT $%d OFFSET $%d", argIndex, argIndex+1)
	args = append(args, limit, offset)

	res := []JobRecord{}

	rows, err := pgDB.Handle.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var r JobRecord
		if err := rows.Scan(&r.JobID, &r.Status, &r.LastUpdate, &r.ProcessID, &r.Submitter); err != nil {
			return nil, err
		}
		res = append(res, r)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (pgDB *PostgresDB) Close() error {
	return pgDB.Handle.Close()
}

func (pgDB *PostgresDB) GetNonTerminalJobs() ([]JobRecord, error) {
	query := `
        SELECT id, status, updated, mode, host, host_job_id, process_id, submitter
        FROM jobs
        WHERE status NOT IN ('successful','failed','dismissed','lost')
        ORDER BY updated DESC
    `
	rows, err := pgDB.Handle.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobRecord{}
	for rows.Next() {
		var r JobRecord
		if err := rows.Scan(
			&r.JobID,
			&r.Status,
			&r.LastUpdate,
			&r.Mode,
			&r.Host,
			&r.HostJobID,
			&r.ProcessID,
			&r.Submitter,
		); err != nil {
			return nil, err
		}
		res = append(res, r)
	}
	return res, rows.Err()
}
