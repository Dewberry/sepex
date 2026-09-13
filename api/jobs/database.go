package jobs

import (
	"fmt"
	"os"
	"time"
)

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so that a record can
// be read the same way whether it came from a single lookup or a listing.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// Database interface abstracts database operations
type Database interface {
	addJob(jid, status, mode, host, hostJobID, processID, submitter string, tags []string, updated time.Time) error
	updateJobRecord(jid, status string, now time.Time) error

	updateJobHostId(jid, hostJobID string) error
	GetNonTerminalJobs() ([]JobRecord, error)

	GetJob(jid string) (JobRecord, bool, error)
	CheckJobExist(jid string) (bool, error)
	GetJobs(limit, offset int, processIDs, statuses, submitters, tags []string) ([]JobRecord, error)

	// Job group writes are exported, unlike the job writes above. A job writes
	// its own record from this package, but a group is submitted from the
	// handlers package, where the process catalog it needs lives.
	AddJobGroup(rec JobGroupRecord) error
	AddJobGroupMember(groupID string, position int, jobID, createError string) error
	UpdateJobGroupSubmitted(groupID string, submitted time.Time, message string) error
	UpdateJobGroupDismissed(groupID string, dismissed time.Time) error

	GetJobGroup(groupID string) (JobGroupRecord, bool, error)
	GetJobGroupMembers(groupID string, limit, offset int, statuses []string) ([]JobGroupMember, error)
	// GetJobGroupSummary counts members by status and reports the latest member
	// update, which is what a group's own updated time is derived from.
	GetJobGroupSummary(groupID string) (JobGroupSummary, time.Time, error)
	// GetJobGroupMemberJobIDs returns the members that have a job, in
	// submission order.
	GetJobGroupMemberJobIDs(groupID string) ([]string, error)
	// GetSubmittingJobGroups returns groups whose submission never finished,
	// which after a restart means it was interrupted.
	GetSubmittingJobGroups() ([]JobGroupRecord, error)

	Close() error
}

func NewDatabase(dbType string) (db Database, err error) {

	switch dbType {
	case "sqlite":
		dbPath, exist := os.LookupEnv("SQLITE_DB_PATH")
		if !exist {
			return nil, fmt.Errorf("env variable SQLITE_DB_PATH not set")
		}
		db, err = NewSQLiteDB(dbPath)
	case "postgres":
		connString, exist := os.LookupEnv("POSTGRES_CONN_STRING")
		if !exist {
			return nil, fmt.Errorf("env variable POSTGRES_CONN_STRING not set")
		}
		db, err = NewPostgresDB(connString)
	default:
		return nil, fmt.Errorf("unsupported database type: %s", dbType)
	}

	if err != nil {
		return nil, err
	}

	return db, nil
}
