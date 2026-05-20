package sync

import "testing"

func TestMungedFromCwd(t *testing.T) {
	cases := []struct {
		cwd, want string
	}{
		{"/home/alice/Git/github.com/alice/outpost", "-home-alice-Git-github-com-alice-outpost"},
		{"/home/u", "-home-u"},
		{"/Users/bob/work", "-Users-bob-work"},
	}
	for _, c := range cases {
		if got := MungedFromCwd(c.cwd); got != c.want {
			t.Errorf("MungedFromCwd(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

func TestCwdFromMungedSimple(t *testing.T) {
	got := CwdFromMunged("-home-alice-foo")
	want := "/home/alice/foo"
	if got != want {
		t.Fatalf("CwdFromMunged: got %q want %q", got, want)
	}
}
