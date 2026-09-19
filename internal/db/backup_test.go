package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBackupIncludesCommittedWAL(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	job := &Job{ID: "preserved", LinearIssueID: "TEST-1", State: StateDone, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := database.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backup.db")
	if err := database.Backup(path); err != nil {
		t.Fatal(err)
	}
	if err := database.Backup(path); err == nil {
		t.Fatal("overwrote backup")
	}
	backup, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	preserved, err := backup.GetJob(job.ID)
	if err != nil || preserved == nil || preserved.State != StateDone {
		t.Fatalf("snapshot lost committed job: %+v %v", preserved, err)
	}
}
