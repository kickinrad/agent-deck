package statedb

import "testing"

func TestOpenReadOnlyLiveSeesWALAndRejectsWrites(t *testing.T) {
	db := newTestDB(t)
	if err := db.SaveInstance(&InstanceRow{ID: "live", Title: "committed in WAL", Tool: "codex"}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnlyLive(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	row, err := reader.LoadInstanceByID("live")
	if err != nil || row == nil || row.Title != "committed in WAL" {
		t.Fatalf("live row = %#v, err = %v", row, err)
	}
	if err := reader.SaveInstance(&InstanceRow{ID: "forbidden"}); err == nil {
		t.Fatal("read-only connection allowed write")
	}
}
