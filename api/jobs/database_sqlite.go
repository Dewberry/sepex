package jobs

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

type SQLiteDB struct {
	Handle *sql.DB
}

// Initialize the database.
// Creates intermediate directories if not exist.
func NewSQLiteDB(dbPath string) (*SQLiteDB, error) {

	// Create directory structure if it doesn't exist
	dir := filepath.Dir(dbPath)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return nil, err
	}

	h, err := sql.Open("sqlite", dbPath+"?mode=rwc&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	// Set WAL mode (not strictly necessary each time because it's persisted in the database).
	// Set busy timeout, so concurrent writers wait on each other instead of erroring immediately,
	// this is per connection setting so necessary each time
	// Set sync mode to normal which is completely safe in WAL and can speed up concurrency
	// It maybe a good idea to make db such that only go can write to it.

	if err != nil {
		return nil, fmt.Errorf("could not open %s Delete the existing database to start with a new database. Error: %s", dbPath, err.Error())
	}

	if h == nil {
		return nil, fmt.Errorf("db nil")
	}

	db := SQLiteDB{Handle: h}
	err = db.createTables()
	if err != nil {
		return nil, err
	}
	return &db, nil
}

// Create tables in the database if they do not exist already
func (sqliteDB *SQLiteDB) createTables() error {

	// SQLite does not have a built-in ENUM type or array type.
	// SQLite doesn't enforce the length of the VARCHAR datatype, therefore not using something like VARCHAR(30).
	// SQLite's concurrency control is based on transactions, not connections. A connection to a SQLite database does not inherently acquire a lock.
	// Locks are acquired when a transaction is started and released when the transaction is committed or rolled back.

	// indices needed to speedup
	// fetching jobs for a particular process id
	// providing job-lists ordered by time

	queryJobs := `
	CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		updated TIMESTAMP NOT NULL,
		mode TEXT NOT NULL,
		host TEXT NOT NULL,
		process_id TEXT NOT NULL,
		submitter TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_jobs_updated ON jobs(updated);
	CREATE INDEX IF NOT EXISTS idx_jobs_process_id ON jobs(process_id);
	CREATE INDEX IF NOT EXISTS idx_jobs_submitter ON jobs(submitter);
	`

	_, err := sqliteDB.Handle.Exec(queryJobs)
	if err != nil {
		return fmt.Errorf("error creating tables: %s", err)
	}
	return nil
}

// Add job to the database. Will return error if job exist.
func (sqliteDB *SQLiteDB) addJob(jid, status, mode, host, processID, submitter string, updated time.Time) error {
	query := `INSERT INTO jobs (id, status, updated, mode, host, process_id, submitter) VALUES (?, ?, ?, ?, ?, ?, ?)`

	_, err := sqliteDB.Handle.Exec(query, jid, status, updated, mode, host, processID, submitter)
	if err != nil {
		return err
	}
	return nil
}

// Update status and time of a job.
func (sqliteDB *SQLiteDB) updateJobRecord(jid, status string, now time.Time) error {
	query := `UPDATE jobs SET status = ?, updated = ? WHERE id = ?`
	_, err := sqliteDB.Handle.Exec(query, status, now, jid)
	if err != nil {
		return err
	}
	return nil
}

// Get Job Record from database given a job id.
// If job do not exists, or error encountered bool would be false.
// Similar behavior as key exist in hashmap.
func (sqliteDB *SQLiteDB) GetJob(jid string) (JobRecord, bool, error) {
	query := `SELECT * FROM jobs WHERE id = ?`

	jr := JobRecord{}

	row := sqliteDB.Handle.QueryRow(query, jid)
	err := row.Scan(&jr.JobID, &jr.Status, &jr.LastUpdate, &jr.Mode, &jr.Host, &jr.ProcessID, &jr.Submitter)
	if err != nil {
		if err == sql.ErrNoRows {
			return JobRecord{}, false, nil
		} else {
			log.Error(err)
			return JobRecord{}, false, err
		}
	}
	return jr, true, nil
}

// Check if a job exists in database.
func (sqliteDB *SQLiteDB) CheckJobExist(jid string) (bool, error) {
	query := `SELECT id FROM jobs WHERE id = ?`

	js := JobRecord{}

	row := sqliteDB.Handle.QueryRow(query, jid)
	err := row.Scan(&js.JobID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		} else {
			return false, err
		}
	}
	return true, nil
}

// Assumes query parameters are valid
func (sqliteDB *SQLiteDB) GetJobs(limit, offset int, processIDs, statuses []string, submitters []string) ([]JobRecord, error) {
	baseQuery := `SELECT id, status, updated, process_id, submitter FROM jobs`
	whereClauses := []string{}
	args := []interface{}{}

	if len(processIDs) > 0 {
		placeholders := strings.Repeat("?,", len(processIDs)-1) + "?"
		whereClauses = append(whereClauses, fmt.Sprintf("process_id IN (%s)", placeholders))
		for _, pid := range processIDs {
			args = append(args, pid)
		}
	}

	if len(statuses) > 0 {
		placeholders := strings.Repeat("?,", len(statuses)-1) + "?"
		whereClauses = append(whereClauses, fmt.Sprintf("status IN (%s)", placeholders))
		for _, st := range statuses {
			args = append(args, st)
		}
	}

	if len(submitters) > 0 {
		placeholders := strings.Repeat("?,", len(submitters)-1) + "?"
		whereClauses = append(whereClauses, fmt.Sprintf("submitter IN (%s)", placeholders))
		for _, sb := range submitters {
			args = append(args, sb)
		}
	}

	if len(whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := baseQuery + ` ORDER BY updated DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	res := []JobRecord{}

	rows, err := sqliteDB.Handle.Query(query, args...)
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

func (sqliteDB *SQLiteDB) Close() error {
	return sqliteDB.Handle.Close()
}
