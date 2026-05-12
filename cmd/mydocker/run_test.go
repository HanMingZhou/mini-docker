package main

import (
	"fmt"
	"testing"
)

func TestParseVolumes(t *testing.T) {
	cases := []struct {
		in      []string
		wantErr bool
	}{
		{[]string{}, false},
		{[]string{"/host:/c"}, false},
		{[]string{"/host:/c:ro"}, false},
		{[]string{"/host:/c:rw"}, false},
		{[]string{"rel:/c"}, true},
		{[]string{"/host:c"}, true},
		{[]string{"/host:/c:wtf"}, true},
		{[]string{"/host"}, true},
	}
	for _, c := range cases {
		_, err := parseVolumes(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseVolumes(%v) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
	}
}
func TestGetParseVolumes(t *testing.T) {
	cases := []struct {
		in      []string
		wantErr bool
	}{

		{[]string{"/host:/c:ro"}, false},
	}
	for _, c := range cases {
		out, err := parseVolumes(c.in)
		if (err != nil) != c.wantErr {
			fmt.Println(out)
		}
	}
}

func TestValidateEnvs(t *testing.T) {
	if err := validateEnvs([]string{"A=1", "B=hello world"}); err != nil {
		t.Fatal(err)
	}
	if err := validateEnvs([]string{"nokey"}); err == nil {
		t.Fatal("want err for missing =")
	}
	if err := validateEnvs([]string{"=value"}); err == nil {
		t.Fatal("want err for empty key")
	}
}

func TestNewID(t *testing.T) {
	fmt.Println(newID())
}
