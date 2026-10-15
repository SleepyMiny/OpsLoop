package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}
	if got := Truncate("exactly10!", 10); got != "exactly10!" {
		t.Fatalf("string at limit changed: %q", got)
	}
	if got := Truncate("abcdefghijk", 10); got != "abcdefg..." {
		t.Fatalf("ascii cut: got %q", got)
	}

	// 10 Hangul runes are 30 bytes; the limit counts runes, not bytes.
	ko := strings.Repeat("가", 10)
	if got := Truncate(ko, 10); got != ko {
		t.Fatalf("multibyte string within rune limit changed: %q", got)
	}
	got := Truncate(strings.Repeat("가", 11), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("cut split a rune: %q", got)
	}
	if want := strings.Repeat("가", 7) + "..."; got != want {
		t.Fatalf("multibyte cut: got %q want %q", got, want)
	}
	if n := utf8.RuneCountInString(got); n != 10 {
		t.Fatalf("rune count = %d, want 10", n)
	}
}

func TestCapList(t *testing.T) {
	in := []string{"a", "b", "c"}
	if got := CapList(in, 3); !reflect.DeepEqual(got, in) {
		t.Fatalf("list at limit changed: %v", got)
	}
	if got := CapList(in[:1], 3); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("short list changed: %v", got)
	}

	long := []string{"a", "b", "c", "d", "e"}
	got := CapList(long, 3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if want := []string{"a", "b", TruncatedListMarker}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got[2] != "...truncated" {
		t.Fatalf("marker = %q", got[2])
	}
	if long[2] != "c" {
		t.Fatalf("input mutated: %v", long)
	}
}
