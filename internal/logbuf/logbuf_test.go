package logbuf

import "testing"

func TestInstanceStoreRoutesOnlyExactInstanceTag(t *testing.T) {
	store := NewInstanceStore(2)
	store.Register("家庭服务器")
	store.Register("另一台")

	store.AppendGlobal("info global management UI listening")
	store.AppendGlobal("debug [f24695] send heartbeat to server")
	store.AppendGlobal("info [frp-more:另一台] [run-id] login to server success")
	store.AppendGlobal("info [frp-more:家庭服务器] [run-id] login to server success")
	store.AppendGlobal("debug [frp-more:家庭服务器] [run-id] receive heartbeat from server")
	store.AppendGlobal("warn [frp-more:家庭服务器] reconnecting")

	lines, dropped, ok := store.Snapshot("家庭服务器")
	if !ok {
		t.Fatal("instance stream was not created")
	}
	if len(lines) != 2 || dropped != 1 {
		t.Fatalf("got %d lines and %d dropped, want 2 lines and 1 dropped", len(lines), dropped)
	}
	for _, line := range lines {
		if line == "info [frp-more:另一台] [run-id] login to server success" || line == "info global management UI listening" {
			t.Fatalf("global or other-instance log leaked into stream: %q", line)
		}
	}
}

func TestInstanceStoreRenameKeepsBuffer(t *testing.T) {
	store := NewInstanceStore(10)
	store.Register("old")
	store.Append("old", "first")
	store.Rename("old", "new")

	if _, _, ok := store.Snapshot("old"); ok {
		t.Fatal("old instance name still exists after rename")
	}
	lines, _, ok := store.Snapshot("new")
	if !ok || len(lines) != 1 || lines[0] != "first" {
		t.Fatalf("renamed stream lost its buffer: %#v, ok=%v", lines, ok)
	}
}
