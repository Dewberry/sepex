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

func joinTags(tags []string) string { return strings.Join(tags, ",") }
func splitTags(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
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
		host_job_id TEXT NOT NULL DEFAULT '',
    process_id TEXT NOT NULL,
    submitter TEXT NOT NULL DEFAULT '',
    tags TEXT NOT NULL DEFAULT ''
);

	CREATE INDEX IF NOT EXISTS idx_jobs_updated ON jobs(updated);
	CREATE INDEX IF NOT EXISTS idx_jobs_process_id ON jobs(process_id);
	CREATE INDEX IF NOT EXISTS idx_jobs_submitter ON jobs(submitter);

`
	_, err := sqliteDB.Handle.Exec(queryJobs)
	if err != nil {
		return fmt.Errorf("error creating tables: %s", err)
	}

	// Job groups. The jobs table is not altered, which matters more here than
	// on Postgres: SQLite has no ADD COLUMN IF NOT EXISTS, so this backend has
	// no migration path for an existing database.
	//
	// A member is keyed by its position rather than by its job, so that a
	// member whose job could not be created still has a row to carry the
	// reason. job_id is therefore nullable, and unique only among the members
	// that have one, which SQLite allows because it treats NULLs in a unique
	// index as distinct.
	queryJobGroups := `
CREATE TABLE IF NOT EXISTS job_groups (
    id TEXT PRIMARY KEY,
    submitter TEXT NOT NULL DEFAULT '',
    tags TEXT NOT NULL DEFAULT '',
    requested INTEGER NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    created TIMESTAMP NOT NULL,
    submitted TIMESTAMP,
    dismissed TIMESTAMP
);

	CREATE INDEX IF NOT EXISTS idx_job_groups_created ON job_groups(created);

CREATE TABLE IF NOT EXISTS job_group_members (
    group_id TEXT NOT NULL REFERENCES job_groups(id),
    position INTEGER NOT NULL,
    job_id TEXT REFERENCES jobs(id),
    error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (group_id, position)
);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_job_group_members_job ON job_group_members(job_id);

`
	_, err = sqliteDB.Handle.Exec(queryJobGroups)
	if err != nil {
		return fmt.Errorf("error creating job group tables: %s", err)
	}

	return nil
}

// Add job to the database. Will return error if job exist.
func (sqliteDB *SQLiteDB) addJob(jid, status, mode, host, hostJobID, processID, submitter string, tags []string, updated time.Time) error {
	query := `INSERT INTO jobs (id, status, updated, mode, host, host_job_id, process_id, submitter, tags) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := sqliteDB.Handle.Exec(query, jid, status, updated, mode, host, hostJobID, processID, submitter, joinTags(tags))
	return err
}

// Update host job id of a job.
func (sqliteDB *SQLiteDB) updateJobHostId(jid, hostJobID string) error {
	query := `UPDATE jobs SET host_job_id = ? WHERE id = ?`
	_, err := sqliteDB.Handle.Exec(query, hostJobID, jid)
	return err
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
	query := `SELECT id, status, updated, mode, host, host_job_id, process_id, submitter, tags FROM jobs WHERE id = ?`
	jr := JobRecord{}
	var tagsStr string
	row := sqliteDB.Handle.QueryRow(query, jid)
	err := row.Scan(&jr.JobID, &jr.Status, &jr.LastUpdate, &jr.Mode, &jr.Host, &jr.HostJobID, &jr.ProcessID, &jr.Submitter, &tagsStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return JobRecord{}, false, nil
		}
		log.Error(err)
		return JobRecord{}, false, err
	}
	jr.Tags = splitTags(tagsStr)
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
func (sqliteDB *SQLiteDB) GetJobs(limit, offset int, processIDs, statuses, submitters, tags []string) ([]JobRecord, error) {
	baseQuery := `SELECT id, status, updated, process_id, submitter, tags FROM jobs`
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
	// Tag filtering: prefix match — e.g. "v1" matches "v1", "v1.2", "v1-beta"
	// Pattern: wrap column in commas, then match ",<prefix>%," to anchor at tag boundaries
	for _, tag := range tags {
		whereClauses = append(whereClauses, `(',' || LOWER(tags) || ',') LIKE LOWER(?)`)
		args = append(args, "%,"+strings.ToLower(tag)+"%,%")
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
		var tagsStr string
		if err := rows.Scan(&r.JobID, &r.Status, &r.LastUpdate, &r.ProcessID, &r.Submitter, &tagsStr); err != nil {
			return nil, err
		}
		r.Tags = splitTags(tagsStr)
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

func (sqliteDB *SQLiteDB) GetNonTerminalJobs() ([]JobRecord, error) {
	query := `
        SELECT id, status, updated, mode, host, host_job_id, process_id, submitter
        FROM jobs
        WHERE status NOT IN ('successful','failed','dismissed','lost')
        ORDER BY updated DESC
    `
	rows, err := sqliteDB.Handle.Query(query)
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

// ---------------------------
// Job groups
// ---------------------------

func (sqliteDB *SQLiteDB) AddJobGroup(rec JobGroupRecord) error {
	query := `INSERT INTO job_groups (id, submitter, tags, requested, message, created) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := sqliteDB.Handle.Exec(query, rec.GroupID, rec.Submitter, joinTags(rec.Tags), rec.Requested, rec.Message, rec.Created)
	return err
}

// AddJobGroupMember records one position in a group. An empty jobID stores no
// job, which is how a member whose job could not be created keeps its place in
// the group and carries the reason instead.
func (sqliteDB *SQLiteDB) AddJobGroupMember(groupID string, position int, jobID, createError string) error {
	query := `INSERT INTO job_group_members (group_id, position, job_id, error) VALUES (?, ?, ?, ?)`
	_, err := sqliteDB.Handle.Exec(query, groupID, position, sql.NullString{String: jobID, Valid: jobID != ""}, createError)
	return err
}

func (sqliteDB *SQLiteDB) UpdateJobGroupSubmitted(groupID string, submitted time.Time, message string) error {
	query := `UPDATE job_groups SET submitted = ?, message = ? WHERE id = ?`
	_, err := sqliteDB.Handle.Exec(query, submitted, message, groupID)
	return err
}

func (sqliteDB *SQLiteDB) UpdateJobGroupDismissed(groupID string, dismissed time.Time) error {
	query := `UPDATE job_groups SET dismissed = ? WHERE id = ?`
	_, err := sqliteDB.Handle.Exec(query, dismissed, groupID)
	return err
}

const sqliteJobGroupColumns = `id, submitter, tags, requested, message, created, submitted, dismissed`

// scanJobGroupSQLite reads one job_groups row selected in
// sqliteJobGroupColumns order.
func scanJobGroupSQLite(sc rowScanner) (JobGroupRecord, error) {
	var rec JobGroupRecord
	var tagsStr string
	var submitted, dismissed sql.NullTime

	err := sc.Scan(&rec.GroupID, &rec.Submitter, &tagsStr, &rec.Requested, &rec.Message, &rec.Created, &submitted, &dismissed)
	if err != nil {
		return JobGroupRecord{}, err
	}

	rec.Tags = splitTags(tagsStr)
	if submitted.Valid {
		t := submitted.Time
		rec.Submitted = &t
	}
	if dismissed.Valid {
		t := dismissed.Time
		rec.Dismissed = &t
	}
	return rec, nil
}

// Get a group record from the database given a group id.
// If the group does not exist, or an error is encountered, bool would be false.
func (sqliteDB *SQLiteDB) GetJobGroup(groupID string) (JobGroupRecord, bool, error) {
	query := `SELECT ` + sqliteJobGroupColumns + ` FROM job_groups WHERE id = ?`

	rec, err := scanJobGroupSQLite(sqliteDB.Handle.QueryRow(query, groupID))
	if err != nil {
		if err == sql.ErrNoRows {
			return JobGroupRecord{}, false, nil
		}
		log.Error(err)
		return JobGroupRecord{}, false, err
	}
	return rec, true, nil
}

func (sqliteDB *SQLiteDB) GetSubmittingJobGroups() ([]JobGroupRecord, error) {
	query := `SELECT ` + sqliteJobGroupColumns + ` FROM job_groups WHERE submitted IS NULL ORDER BY created`

	rows, err := sqliteDB.Handle.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobGroupRecord{}
	for rows.Next() {
		rec, err := scanJobGroupSQLite(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, rec)
	}
	return res, rows.Err()
}

// Assumes query parameters are valid
func (sqliteDB *SQLiteDB) GetJobGroupMembers(groupID string, limit, offset int, statuses []string) ([]JobGroupMember, error) {
	// A left join, because a member whose job could not be created has a row
	// here but no job to join to.
	query := `SELECT m.position, m.job_id, m.error, j.process_id, j.status, j.updated, j.tags
	FROM job_group_members m
	LEFT JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = ?`

	args := []interface{}{groupID}

	jobStatuses, includeNotCreated := splitMemberStatusFilter(statuses)
	if len(jobStatuses) > 0 {
		placeholders := strings.Repeat("?,", len(jobStatuses)-1) + "?"
		clause := fmt.Sprintf("j.status IN (%s)", placeholders)
		if includeNotCreated {
			clause = "(" + clause + " OR m.job_id IS NULL)"
		}
		query += " AND " + clause
		for _, st := range jobStatuses {
			args = append(args, st)
		}
	} else if includeNotCreated {
		query += " AND m.job_id IS NULL"
	}

	query += ` ORDER BY m.position LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := sqliteDB.Handle.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobGroupMember{}
	for rows.Next() {
		var m JobGroupMember
		var jobID, processID, status, tagsStr sql.NullString
		var updated sql.NullTime

		if err := rows.Scan(&m.Position, &jobID, &m.Error, &processID, &status, &updated, &tagsStr); err != nil {
			return nil, err
		}

		m.JobID = jobID.String
		m.ProcessID = processID.String
		m.Status = status.String
		if updated.Valid {
			t := updated.Time
			m.LastUpdate = &t
		}
		m.Tags = splitTags(tagsStr.String)
		res = append(res, m)
	}
	return res, rows.Err()
}

// GetJobGroupSummary counts members by status. Members with no job are left out
// of the buckets here and counted from the requested total instead, so that
// members whose creation failed and members never attempted are counted the
// same way.
func (sqliteDB *SQLiteDB) GetJobGroupSummary(groupID string) (JobGroupSummary, time.Time, error) {
	query := `SELECT j.status, COUNT(*)
	FROM job_group_members m
	LEFT JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = ?
	GROUP BY j.status`

	rows, err := sqliteDB.Handle.Query(query, groupID)
	if err != nil {
		return JobGroupSummary{}, time.Time{}, err
	}
	defer rows.Close()

	var summary JobGroupSummary
	for rows.Next() {
		var status sql.NullString
		var count int

		if err := rows.Scan(&status, &count); err != nil {
			return JobGroupSummary{}, time.Time{}, err
		}
		if status.Valid {
			summary.addStatus(status.String, count)
		}
	}
	if err := rows.Err(); err != nil {
		return JobGroupSummary{}, time.Time{}, err
	}

	latest, err := sqliteDB.latestJobGroupUpdate(groupID)
	if err != nil {
		return JobGroupSummary{}, time.Time{}, err
	}

	return summary, latest, nil
}

// latestJobGroupUpdate reports when a group's members last changed.
//
// The time is read as a plain column rather than as MAX(updated), because
// SQLite loses a column's declared type through an aggregate and the driver
// then returns text where a timestamp is expected. Postgres does the same thing
// the same way, so that the two backends read alike.
func (sqliteDB *SQLiteDB) latestJobGroupUpdate(groupID string) (time.Time, error) {
	query := `SELECT j.updated
	FROM job_group_members m
	JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = ?
	ORDER BY j.updated DESC
	LIMIT 1`

	var latest time.Time
	err := sqliteDB.Handle.QueryRow(query, groupID).Scan(&latest)
	if err == sql.ErrNoRows {
		// A group whose members have all yet to be created has no update time.
		return time.Time{}, nil
	}
	return latest, err
}

func (sqliteDB *SQLiteDB) GetJobGroupMemberJobIDs(groupID string) ([]string, error) {
	query := `SELECT job_id FROM job_group_members WHERE group_id = ? AND job_id IS NOT NULL ORDER BY position`

	rows, err := sqliteDB.Handle.Query(query, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []string{}
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return nil, err
		}
		res = append(res, jobID)
	}
	return res, rows.Err()
}
