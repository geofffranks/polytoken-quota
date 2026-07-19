package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWriteFileCreatesParentsAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "cfg.yaml")
	WriteFile(t, path, "hello\n")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("content = %q", data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != FilePerm {
		t.Fatalf("file perm = %v err %v want %v", info.Mode().Perm(), err, FilePerm)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != DirPerm {
		t.Fatalf("dir perm = %v err %v want %v", info.Mode().Perm(), err, DirPerm)
	}
}

func TestSnapshotRoundTripsIdentical(t *testing.T) {
	dir := t.TempDir()
	WriteFile(t, filepath.Join(dir, "x", "one.yaml"), "a: 1\n")
	WriteFile(t, filepath.Join(dir, "two.md"), "body\n")

	before := Snapshot(t, dir)
	after := Snapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot not stable: before=%v after=%v", before, after)
	}
	if len(before) != 2 {
		t.Fatalf("want 2 files, got %d", len(before))
	}
	if _, ok := before["x/one.yaml"]; !ok {
		t.Fatalf("missing x/one.yaml in %v", before)
	}
}

func TestFileSnapshotEqualDetectsChanges(t *testing.T) {
	a := FileSnapshot{Content: []byte("x"), Mode: 0o600}
	b := FileSnapshot{Content: []byte("x"), Mode: 0o600}
	c := FileSnapshot{Content: []byte("y"), Mode: 0o600}
	if !a.Equal(b) {
		t.Fatal("identical snapshots not equal")
	}
	if a.Equal(c) {
		t.Fatal("different content reported equal")
	}
}

func TestFakeClockAdvances(t *testing.T) {
	start := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("now = %v want %v", c.Now(), start)
	}
	c.Advance(5 * time.Second)
	want := start.Add(5 * time.Second)
	if !c.Now().Equal(want) {
		t.Fatalf("now = %v want %v", c.Now(), want)
	}
	c.Set(start)
	if !c.Now().Equal(start) {
		t.Fatalf("set failed: %v", c.Now())
	}
}

func TestCommandSpyReplaysResults(t *testing.T) {
	spy := NewCommandSpy(
		CommandResult{Stdout: "first", ExitCode: 0},
		CommandResult{Stdout: "second", ExitCode: 2},
	)
	r1 := spy.Run("/work", "polytoken", "doctor")
	if r1.Stdout != "first" || r1.ExitCode != 0 {
		t.Fatalf("r1 = %+v", r1)
	}
	r2 := spy.Run("/work", "polytoken", "doctor")
	if r2.Stdout != "second" || r2.ExitCode != 2 {
		t.Fatalf("r2 = %+v", r2)
	}
	// Exhausted: last result reused.
	r3 := spy.Run("/work", "polytoken", "doctor")
	if r3.Stdout != "second" {
		t.Fatalf("r3 = %+v", r3)
	}
	calls := spy.Calls()
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	if calls[0].Dir != "/work" || calls[0].Name != "polytoken" {
		t.Fatalf("call0 = %+v", calls[0])
	}
	spy.FailWith(errors.New("boom"))
	r4 := spy.Run("/work", "x")
	if r4.Err == nil {
		t.Fatal("expected injected error")
	}
}

func TestFaultFSInjectsAfterThreshold(t *testing.T) {
	var f FaultFS
	f.FailAfter = 2
	dir := t.TempDir()
	if err := f.MkdirAll(filepath.Join(dir, "ok"), DirPerm); err != nil {
		t.Fatalf("op1: %v", err)
	}
	if err := f.WriteFile(filepath.Join(dir, "ok", "f"), []byte("x"), FilePerm); err != nil {
		t.Fatalf("op2: %v", err)
	}
	if err := f.WriteFile(filepath.Join(dir, "ok", "g"), []byte("y"), FilePerm); !errors.Is(err, ErrFaultInjected) {
		t.Fatalf("op3: want injected, got %v", err)
	}
}
