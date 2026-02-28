package jobs

import (
	"fmt"
	"os"
	"time"
)

// Database interface abstracts database operations
type Database interface {
	addJob(jid, status, mode, host, hostJobID, processID, submitter string, updated time.Time) error
	updateJobRecord(jid, status string, now time.Time) error

	updateJobHostId(jid, hostJobID string) error
	GetNonTerminalJobs() ([]JobRecord, error)

	GetJob(jid string) (JobRecord, bool, error)
	CheckJobExist(jid string) (bool, error)
	GetJobs(limit, offset int, processIDs, statuses, submitters []string) ([]JobRecord, error)
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
