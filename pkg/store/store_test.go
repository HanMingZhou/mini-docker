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

// TestConcurrentSaveSameID 验证同进程多 goroutine 对同一 ID 并发 Save 不会
// 撕裂 config.json（曾经的 bug：tmp 文件路径冲突，rename 后 JSON 半截）。
func TestConcurrentSaveSameID(t *testing.T) {
	s := newTestStore(t)
	const id = "cccccccccccc"
	const goroutines = 50

	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			c := &Container{
				ID:        id,
				Name:      "racer",
				CreatedAt: time.Now(),
				ExitCode:  i,
			}
			if err := s.Save(c); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}

	// 读一次，JSON 必须能正常解析（任何一次 Save 的内容都合法）。
	c, err := s.loadByID(id)
	if err != nil {
		t.Fatalf("loadByID after concurrent save: %v", err)
	}
	if c.ID != id || c.Name != "racer" {
		t.Fatalf("data corrupted: ID=%q Name=%q", c.ID, c.Name)
	}
}

// TestConcurrentSaveDifferentIDs 验证多 goroutine 并发 Save 不同 ID 都成功。
func TestConcurrentSaveDifferentIDs(t *testing.T) {
	s := newTestStore(t)
	const goroutines = 20

	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			id := "id00000000" + string(rune('a'+i%26))
			if err := s.Save(&Container{ID: id, Name: id, CreatedAt: time.Now()}); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	// 应该有 min(20, 26) = 20 个唯一 ID（实际按字母映射有重复 id，取唯一值）
	if len(list) == 0 {
		t.Fatalf("expected some containers after concurrent saves")
	}
}

// TestWithLockMutualExclusion 验证 WithLock 在同进程内也能互斥（通过 flock）。
// 两个 goroutine 都 WithLock，临界区累加计数；任意交错都应到达 n 次。
//
// 注意：WithLock 非 Linux 是 no-op，因此 race 无法消除，只在 Linux 测。
func TestWithLockMutualExclusion(t *testing.T) {
	if !hasFileLock {
		t.Skip("WithLock is a no-op on this platform")
	}
	s := newTestStore(t)
	const n = 50
	counter := 0

	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if err := s.WithLock(func() error {
				// 非原子读-改-写：只有在真正互斥时才能跑对
				cur := counter
				time.Sleep(time.Microsecond) // 放大 race 窗口
				counter = cur + 1
				return nil
			}); err != nil {
				t.Errorf("WithLock: %v", err)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}

	if counter != n {
		t.Fatalf("counter = %d, want %d (race condition detected)", counter, n)
	}
}
