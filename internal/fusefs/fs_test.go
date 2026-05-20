package fusefs

import "testing"

func TestClassifySessionPath(t *testing.T) {
	cases := []struct {
		rel    string
		munged string
		id     string
		ok     bool
	}{
		{"projects/-home-alice-foo/abc.jsonl", "-home-alice-foo", "abc", true},
		{"projects/-home-alice-foo/sub/abc.jsonl", "", "", false},
		{"projects/-home-alice-foo/abc.txt", "", "", false},
		{"todos/foo.json", "", "", false},
		{"projects/-x/.jsonl", "", "", false},
	}
	for _, c := range cases {
		m, id, ok := classifySessionPath(c.rel)
		if ok != c.ok || m != c.munged || id != c.id {
			t.Errorf("classifySessionPath(%q) = (%q, %q, %v); want (%q, %q, %v)", c.rel, m, id, ok, c.munged, c.id, c.ok)
		}
	}
}
