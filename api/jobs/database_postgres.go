package jobs

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
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

// UpdateJobHostId updates the host_job_id of a job
func (db *PostgresDB) updateJobHostId(jid, hostJobID string) error {
	query := `UPDATE jobs SET host_job_id = $2 WHERE id = $1`
	_, err := db.Handle.Exec(query, jid, hostJobID)
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
				host_job_id TEXT NOT NULL DEFAULT '',
				process_id TEXT NOT NULL,
				submitter TEXT NOT NULL DEFAULT '',
				tags TEXT[] NOT NULL DEFAULT '{}'
		);
    CREATE INDEX IF NOT EXISTS idx_jobs_updated ON jobs(updated);
    CREATE INDEX IF NOT EXISTS idx_jobs_process_id ON jobs(process_id);
    CREATE INDEX IF NOT EXISTS idx_jobs_submitter ON jobs(submitter);
`
	_, err := postgresDB.Handle.Exec(queryJobs)
	if err != nil {
		return fmt.Errorf("error creating tables: %s", err)
	}

	// Backfill schema for older databases that predate host_job_id.
	if _, err := postgresDB.Handle.Exec(`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS host_job_id TEXT NOT NULL DEFAULT '';`); err != nil {
		return fmt.Errorf("error adding host_job_id column: %s", err)
	}
	if _, err := postgresDB.Handle.Exec(`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';`); err != nil {
		return fmt.Errorf("error adding tags column: %s", err)
	}

	// Job groups.
	//
	// A member is keyed by its position, so that a
	// member whose job could not be created still has a row to carry the
	// reason. job_id is therefore nullable, and unique only among the members
	// that have one.
	queryJobGroups := `
		CREATE TABLE IF NOT EXISTS job_groups (
				id TEXT PRIMARY KEY,
				submitter TEXT NOT NULL DEFAULT '',
				tags TEXT[] NOT NULL DEFAULT '{}',
				requested INTEGER NOT NULL,
				message TEXT NOT NULL DEFAULT '',
				created TIMESTAMP WITHOUT TIME ZONE NOT NULL,
				submitted TIMESTAMP WITHOUT TIME ZONE,
				dismissed TIMESTAMP WITHOUT TIME ZONE
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
	if _, err := postgresDB.Handle.Exec(queryJobGroups); err != nil {
		return fmt.Errorf("error creating job group tables: %s", err)
	}

	return nil
}

// AddJob adds a new job to the database
func (db *PostgresDB) addJob(jid, status, mode, host, hostJobID, processID, submitter string, tags []string, updated time.Time) error {
	query := `INSERT INTO jobs (id, status, updated, mode, host, host_job_id, process_id, submitter, tags) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	_, err := db.Handle.Exec(query, jid, status, updated, mode, host, hostJobID, processID, submitter, pq.Array(tags))
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
	query := `SELECT id, status, updated, mode, host, host_job_id, process_id, submitter, tags FROM jobs WHERE id = $1`
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
		pq.Array(&jr.Tags),
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
func (pgDB *PostgresDB) GetJobs(limit, offset int, processIDs, statuses, submitters, tags []string) ([]JobRecord, error) {
	baseQuery := `SELECT id, status, updated, process_id, submitter, tags FROM jobs`
	whereClauses := []string{}
	args := []interface{}{}
	argIndex := 1

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
	// Tag filtering: prefix match — each tag term must prefix-match at least one element
	for _, tag := range tags {
		whereClauses = append(whereClauses, fmt.Sprintf("EXISTS (SELECT 1 FROM unnest(tags) t WHERE t ILIKE $%d)", argIndex))
		argIndex++
		args = append(args, tag+"%")
	}

	if len(whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := baseQuery + fmt.Sprintf(" ORDER BY updated DESC LIMIT $%d OFFSET $%d", argIndex, argIndex+1)
	args = append(args, limit, offset)

	rows, err := pgDB.Handle.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobRecord{}
	for rows.Next() {
		var r JobRecord
		if err := rows.Scan(&r.JobID, &r.Status, &r.LastUpdate, &r.ProcessID, &r.Submitter, pq.Array(&r.Tags)); err != nil {
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

// ---------------------------
// Job groups
// ---------------------------

func (pgDB *PostgresDB) AddJobGroup(rec JobGroupRecord) error {
	query := `INSERT INTO job_groups (id, submitter, tags, requested, message, created) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := pgDB.Handle.Exec(query, rec.GroupID, rec.Submitter, pq.Array(rec.Tags), rec.Requested, rec.Message, rec.Created)
	return err
}

// AddJobGroupMember records one position in a group. An empty jobID stores no
// job, which is how a member whose job could not be created keeps its place in
// the group and carries the reason instead.
func (pgDB *PostgresDB) AddJobGroupMember(groupID string, position int, jobID, createError string) error {
	query := `INSERT INTO job_group_members (group_id, position, job_id, error) VALUES ($1, $2, $3, $4)`
	_, err := pgDB.Handle.Exec(query, groupID, position, sql.NullString{String: jobID, Valid: jobID != ""}, createError)
	return err
}

func (pgDB *PostgresDB) UpdateJobGroupSubmitted(groupID string, submitted time.Time, message string) error {
	query := `UPDATE job_groups SET submitted = $2, message = $3 WHERE id = $1`
	_, err := pgDB.Handle.Exec(query, groupID, submitted, message)
	return err
}

func (pgDB *PostgresDB) UpdateJobGroupDismissed(groupID string, dismissed time.Time) error {
	query := `UPDATE job_groups SET dismissed = $2 WHERE id = $1`
	_, err := pgDB.Handle.Exec(query, groupID, dismissed)
	return err
}

const pgJobGroupColumns = `id, submitter, tags, requested, message, created, submitted, dismissed`

// scanJobGroupPG reads one job_groups row selected in pgJobGroupColumns order.
func scanJobGroupPG(sc rowScanner) (JobGroupRecord, error) {
	var rec JobGroupRecord
	var submitted, dismissed sql.NullTime

	err := sc.Scan(&rec.GroupID, &rec.Submitter, pq.Array(&rec.Tags), &rec.Requested, &rec.Message, &rec.Created, &submitted, &dismissed)
	if err != nil {
		return JobGroupRecord{}, err
	}

	if submitted.Valid {
		t := submitted.Time
		rec.Submitted = &t
	}
	if dismissed.Valid {
		t := dismissed.Time
		rec.Dismissed = &t
	}
	if rec.Tags == nil {
		rec.Tags = []string{}
	}
	return rec, nil
}

// GetJobGroup retrieves a group record by id
func (pgDB *PostgresDB) GetJobGroup(groupID string) (JobGroupRecord, bool, error) {
	query := `SELECT ` + pgJobGroupColumns + ` FROM job_groups WHERE id = $1`

	rec, err := scanJobGroupPG(pgDB.Handle.QueryRow(query, groupID))
	if err != nil {
		if err == sql.ErrNoRows {
			return JobGroupRecord{}, false, nil
		}
		return JobGroupRecord{}, false, err
	}
	return rec, true, nil
}

func (pgDB *PostgresDB) GetSubmittingJobGroups() ([]JobGroupRecord, error) {
	query := `SELECT ` + pgJobGroupColumns + ` FROM job_groups WHERE submitted IS NULL ORDER BY created`

	rows, err := pgDB.Handle.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobGroupRecord{}
	for rows.Next() {
		rec, err := scanJobGroupPG(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, rec)
	}
	return res, rows.Err()
}

// Assumes query parameters are valid
func (pgDB *PostgresDB) GetJobGroupMembers(groupID string, limit, offset int, statuses []string) ([]JobGroupMember, error) {
	// A left join, because a member whose job could not be created has a row
	// here but no job to join to.
	query := `SELECT m.position, m.job_id, m.error, j.process_id, j.status, j.updated, j.tags
	FROM job_group_members m
	LEFT JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = $1`

	args := []interface{}{groupID}
	argIndex := 2

	jobStatuses, includeNotCreated := splitMemberStatusFilter(statuses)
	if len(jobStatuses) > 0 {
		placeholders := make([]string, len(jobStatuses))
		for i, st := range jobStatuses {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			argIndex++
			args = append(args, st)
		}
		clause := "j.status IN (" + strings.Join(placeholders, ", ") + ")"
		if includeNotCreated {
			clause = "(" + clause + " OR m.job_id IS NULL)"
		}
		query += " AND " + clause
	} else if includeNotCreated {
		query += " AND m.job_id IS NULL"
	}

	query += fmt.Sprintf(" ORDER BY m.position LIMIT $%d OFFSET $%d", argIndex, argIndex+1)
	args = append(args, limit, offset)

	rows, err := pgDB.Handle.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := []JobGroupMember{}
	for rows.Next() {
		var m JobGroupMember
		var jobID, processID, status sql.NullString
		var updated sql.NullTime

		if err := rows.Scan(&m.Position, &jobID, &m.Error, &processID, &status, &updated, pq.Array(&m.Tags)); err != nil {
			return nil, err
		}

		m.JobID = jobID.String
		m.ProcessID = processID.String
		m.Status = status.String
		if updated.Valid {
			t := updated.Time
			m.LastUpdate = &t
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		res = append(res, m)
	}
	return res, rows.Err()
}

// GetJobGroupSummary counts members by status. Members with no job are left out
// of the buckets here and counted from the requested total instead, so that
// members whose creation failed and members never attempted are counted the
// same way.
func (pgDB *PostgresDB) GetJobGroupSummary(groupID string) (JobGroupSummary, time.Time, error) {
	query := `SELECT j.status, COUNT(*)
	FROM job_group_members m
	LEFT JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = $1
	GROUP BY j.status`

	rows, err := pgDB.Handle.Query(query, groupID)
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

	latest, err := pgDB.latestJobGroupUpdate(groupID)
	if err != nil {
		return JobGroupSummary{}, time.Time{}, err
	}

	return summary, latest, nil
}

// latestJobGroupUpdate reports when a group's members last changed.
//
// The time is read as a plain column rather than as MAX(updated) to match the
// SQLite backend, where a column's declared type is lost through an aggregate
// and the driver returns text where a timestamp is expected.
func (pgDB *PostgresDB) latestJobGroupUpdate(groupID string) (time.Time, error) {
	query := `SELECT j.updated
	FROM job_group_members m
	JOIN jobs j ON j.id = m.job_id
	WHERE m.group_id = $1
	ORDER BY j.updated DESC
	LIMIT 1`

	var latest time.Time
	err := pgDB.Handle.QueryRow(query, groupID).Scan(&latest)
	if err == sql.ErrNoRows {
		// A group whose members have all yet to be created has no update time.
		return time.Time{}, nil
	}
	return latest, err
}

func (pgDB *PostgresDB) GetJobGroupMemberJobIDs(groupID string) ([]string, error) {
	query := `SELECT job_id FROM job_group_members WHERE group_id = $1 AND job_id IS NOT NULL ORDER BY position`

	rows, err := pgDB.Handle.Query(query, groupID)
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
