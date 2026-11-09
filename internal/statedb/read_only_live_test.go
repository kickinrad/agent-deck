package statedb

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyLiveSeesUncheckpointedWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	for _, query := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE evidence (value TEXT)", "PRAGMA wal_checkpoint(TRUNCATE)", "INSERT INTO evidence VALUES ('committed in WAL')"} {
		if _, err := writer.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	immutable, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := immutable.DB().QueryRow("SELECT COUNT(*) FROM evidence").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fixture unexpectedly checkpointed: %d", count)
	}
	if err := immutable.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := OpenReadOnlyLive(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.DB().QueryRow("SELECT COUNT(*) FROM evidence").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("live reader missed WAL: %d", count)
	}
	if _, err := live.DB().Exec("DELETE FROM evidence"); err == nil {
		t.Fatal("read-only connection accepted write")
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.QueryRow("SELECT COUNT(*) FROM evidence").Scan(&count); err != nil || count != 1 {
		t.Fatalf("reader changed rows: count=%d err=%v", count, err)
	}
}

func TestOpenReadOnlyLiveWithoutWALInReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("expected checkpointed database without WAL, got %v", err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	reader, err := OpenReadOnlyLive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.GetMeta("profile"); err != nil {
		t.Fatal(err)
	}
}
