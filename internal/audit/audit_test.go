package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func entry(i int) Entry {
	return Entry{
		Time: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC), Event: EventToolCall, Server: "billing", Transport: "stdio",
		Method: "tools/call", Tool: fmt.Sprintf("tool_%d", i), Decision: "block", Mode: "enforce", Rule: "secrets.arguments",
		Findings: []Finding{{Rule: "secrets.arguments", Action: "block", Location: "arguments.note", SecretType: "aws_access_key_id", Reason: "arguments.note looks like an <AWS> key"}},
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestOneObjectPerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "audit.jsonl")
	l, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 3; i++ {
		if err := l.Write(entry(i)); err != nil {
			t.Fatal(err)
		}
	}
	got := lines(t, path)
	if len(got) != 3 {
		t.Fatalf("%d lines", len(got))
	}
	var e Entry
	if err := json.Unmarshal([]byte(got[2]), &e); err != nil || e.Tool != "tool_2" || e.Findings[0].SecretType != "aws_access_key_id" {
		t.Errorf("%v %+v", err, e)
	}
	if !strings.Contains(got[0], "<AWS>") {
		t.Error("reasons must stay readable: no HTML escaping")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(Options{Path: path, MaxSize: 1000, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 20; i++ {
		if err := l.Write(entry(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(p)
		if err != nil || info.Size() > 1000 {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("only max_files rotated files are kept")
	}
	if last := lines(t, path); !strings.Contains(last[len(last)-1], "tool_19") {
		t.Errorf("the newest entry is in the current file: %v", last)
	}
}

// Several processes share one file: each follows the rotations of the
// others, and no line is lost or torn.
func TestSharedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	var logs []*Log
	for i := 0; i < 3; i++ {
		l, err := Open(Options{Path: path, MaxSize: 4000, MaxFiles: 50})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		logs = append(logs, l)
	}
	var wg sync.WaitGroup
	for w, l := range logs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := l.Write(entry(w*100 + i)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	total := 0
	matches, _ := filepath.Glob(path + "*")
	for _, p := range matches {
		if strings.HasSuffix(p, ".lock") {
			continue
		}
		for _, line := range lines(t, p) {
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("torn line in %s: %q", p, line)
			}
			total++
		}
	}
	if total != 150 {
		t.Errorf("%d entries, want 150", total)
	}
}
