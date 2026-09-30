package main

import "testing"

func TestDirectoryGroup(t *testing.T) {
	cases := []struct {
		path  string
		depth int
		want  string
	}{
		{"/home/m/Projects/x/file.go", 3, "/home/m/Projects"},
		{"/home/m/Projects/x/file.go", 1, "/home"},
		{"/home/m/Projects/x/file.go", 9, "/home/m/Projects/x"},
		{"/tmp/file", 3, "/tmp"},
		{"/file", 3, "/"},
	}
	for _, c := range cases {
		if got := directoryGroup(c.path, c.depth); got != c.want {
			t.Errorf("directoryGroup(%q, %d) = %q, want %q", c.path, c.depth, got, c.want)
		}
	}
}
