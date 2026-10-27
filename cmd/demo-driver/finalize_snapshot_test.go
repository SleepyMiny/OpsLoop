//go:build linux

package main

import (
	"fmt"
	"testing"
)

// A writer may stop immediately after a change. Once two captured revisions
// differ, later identical reads must not erase that observation in this call.
func TestFinalizeObservedRevisionChange(t *testing.T) {
	for _, change := range []string{"guard", "manifest", "evidence", "metadata", "read-error", "config"} {
		t.Run(change, func(t *testing.T) {
			captures := 0
			_, stable := stableFinalWith([]byte("config"), nil, func() finalSnapshot {
				captures++
				s := finalSnapshot{
					Data:     map[string][]byte{"config": []byte("config"), "guard": []byte("guard"), "manifest": []byte("manifest"), "evidence": []byte("evidence")},
					Metadata: map[string]string{"guard": "inode-before"},
					Problems: map[string]string{},
				}
				if change == "read-error" {
					if captures == 1 {
						s.Problems["guard"] = "INPUT_FILE_INVALID"
					}
				} else if captures >= 2 {
					if change == "metadata" {
						s.Metadata["guard"] = "inode-after"
					} else {
						s.Data[change] = []byte("replacement")
					}
				}
				return s
			})
			if stable || captures != 2 {
				t.Fatalf("observed %s change was retried/accepted: stable=%t captures=%d", change, stable, captures)
			}
		})
	}
}

// Snapshot equality is an observation, not a claim that an external writer is
// idle or that the files will remain unchanged after the final read.
func TestFinalizeSameRevisionObservation(t *testing.T) {
	captures, boundary := 0, 0
	_, stable := stableFinalWith([]byte("config"), func(attempt int) {
		boundary++
		if captures != 1 || attempt != 0 {
			t.Fatal("wrong snapshot boundary", captures, attempt)
		}
	}, func() finalSnapshot {
		captures++
		return finalSnapshot{Data: map[string][]byte{"config": []byte("config")}, Metadata: map[string]string{"guard": fmt.Sprint(1)}}
	})
	if !stable || captures != 2 || boundary != 1 {
		t.Fatal("identical revision control failed", captures, boundary, stable)
	}
}
