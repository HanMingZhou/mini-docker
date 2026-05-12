package sandbox

import (
	"testing"
	"time"
)

func TestSaveListResolve(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}

	sb1 := &Sandbox{
		ID:        "aaaaaaaaaaaa",
		Metadata:  Metadata{Name: "alpha", Namespace: "default", UID: "u1"},
		State:     StateNotReady,
		CreatedAt: time.Now().UTC(),
	}
	sb2 := &Sandbox{
		ID:        "abcd12345678",
		Metadata:  Metadata{Name: "bravo", Namespace: "default", UID: "u2"},
		State:     StateNotReady,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.save(sb1); err != nil {
		t.Fatal(err)
	}
	if err := m.save(sb2); err != nil {
		t.Fatal(err)
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}

	// 完整 ID / 前缀 / Name 都能命中
	for _, ref := range []string{"aaaaaaaaaaaa", "alpha"} {
		got, err := m.Resolve(ref)
		if err != nil || got.ID != "aaaaaaaaaaaa" {
			t.Errorf("Resolve(%q): got %+v err=%v", ref, got, err)
		}
	}
	got, err := m.Resolve("abcd")
	if err != nil || got.ID != "abcd12345678" {
		t.Errorf("Resolve prefix: got %+v err=%v", got, err)
	}
	// 前缀 "a" 同时命中两个，要 ambiguous
	if _, err := m.Resolve("a"); err == nil {
		t.Error("expected ambiguous error")
	}
}

func TestRemoveRequiresNotReady(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{
		ID:        "xxxxxxxxxxxx",
		Metadata:  Metadata{Name: "x", Namespace: "d", UID: "u"},
		State:     StateNotReady, // 已停止，可以删
		CreatedAt: time.Now().UTC(),
	}
	_ = m.save(sb)
	if err := m.Remove(sb.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Fatalf("list should be empty, got %d", len(list))
	}
}
