package store

import (
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSaveAndList(t *testing.T) {
	s := newTestStore(t)
	c1 := &Container{ID: "aaaaaaaaaaaa", Name: "first", CreatedAt: time.Now()}
	c2 := &Container{ID: "bbbbbbbbbbbb", Name: "second", CreatedAt: time.Now().Add(time.Second)}
	if err := s.Save(c1); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(c2); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 containers, got %d", len(list))
	}
}

func TestResolve(t *testing.T) {
	s := newTestStore(t)
	_ = s.Save(&Container{ID: "aaaaaaaaaaaa", Name: "alpha"})
	_ = s.Save(&Container{ID: "abcd12345678", Name: "bravo"})

	cases := []struct {
		ref     string
		wantID  string
		wantErr bool
		errSub  string
	}{
		{"alpha", "aaaaaaaaaaaa", false, ""},
		{"bravo", "abcd12345678", false, ""},
		{"aaaaaaaaaaaa", "aaaaaaaaaaaa", false, ""},
		{"abcd", "abcd12345678", false, ""},
		{"a", "", true, "ambiguous"}, // aaaa... 和 abcd... 都以 a 开头
		{"zzz", "", true, "no such"},
		{"", "", true, ""},
	}
	for _, c := range cases {
		got, err := s.Resolve(c.ref)
		if (err != nil) != c.wantErr {
			t.Errorf("Resolve(%q) err=%v wantErr=%v", c.ref, err, c.wantErr)
		}
		if err == nil && got.ID != c.wantID {
			t.Errorf("Resolve(%q).ID = %q, want %q", c.ref, got.ID, c.wantID)
		}
		if err != nil && c.errSub != "" && !strings.Contains(err.Error(), c.errSub) {
			t.Errorf("Resolve(%q) err %q does not contain %q", c.ref, err.Error(), c.errSub)
		}
	}
}

func TestRemove(t *testing.T) {
	s := newTestStore(t)
	_ = s.Save(&Container{ID: "xxxxxxxxxxxx"})
	if err := s.Remove("xxxxxxxxxxxx"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("want empty list after remove, got %d", len(list))
	}
}
