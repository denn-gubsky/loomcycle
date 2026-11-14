package helper

import "testing"

// Program is the template's first word, quoted or not; a placeholder there is
// refused, since a worker could not know what it would run.
func TestProgram_IsTheFirstWord(t *testing.T) {
	for _, c := range []struct{ tmpl, want string }{
		{"ffmpeg -i {in} {out}", "ffmpeg"},
		{"'/opt/my tools/pdftoppm' -png {in} {out_prefix}", "/opt/my tools/pdftoppm"},
	} {
		if got, err := Program(c.tmpl); err != nil || got != c.want {
			t.Errorf("Program(%q) = %q, %v; want %q", c.tmpl, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "{in} -x", "a|b"} {
		if got, err := Program(bad); err == nil {
			t.Errorf("Program(%q) = %q, want an error", bad, got)
		}
	}
}
