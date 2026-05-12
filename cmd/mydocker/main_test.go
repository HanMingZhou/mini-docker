package main

import (
	"fmt"
	"testing"
)

func TestParseMemory(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"100", 100, false},
		{"100k", 100 << 10, false},
		{"100M", 100 << 20, false},
		{"1g", 1 << 30, false},
		{"0", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseMemory(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseMemory(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("parseMemory(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseCPUs(t *testing.T) {
	cases := []struct {
		in         string
		wantQuota  int64
		wantPeriod int64
		wantErr    bool
	}{
		{"", 0, 0, false},
		{"1", 100_000, 100_000, false},
		{"0.5", 50_000, 100_000, false},
		{"2", 200_000, 100_000, false},
		{"0", 0, 0, true},
		{"abc", 0, 0, true},
	}
	for _, c := range cases {
		q, p, err := parseCPUs(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseCPUs(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err == nil && (q != c.wantQuota || p != c.wantPeriod) {
			t.Errorf("parseCPUs(%q) = (%d,%d), want (%d,%d)",
				c.in, q, p, c.wantQuota, c.wantPeriod)
		}
	}
}

func TestCpus(t *testing.T) {
	q, p, err := parseCPUs("1")
	if err != nil {
		panic(err)
	}
	fmt.Println("quota", q)
	fmt.Println("period", p)
}
