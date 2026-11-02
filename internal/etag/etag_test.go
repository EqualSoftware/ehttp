package etag

import "testing"

func TestMatchAndValid(t *testing.T) {
	t.Parallel()

	const opaque = `"1-00000001"`

	tests := []struct {
		name   string
		values []string
		valid  bool
		match  bool
	}{
		{"no field", nil, true, false},
		{"same weak tag", []string{`W/"1-00000001"`}, true, true},
		{"strong form (weak comparison)", []string{opaque}, true, true},
		{"list with the tag", []string{`"a", W/"1-00000001"`}, true, true},
		{"tag with a comma inside", []string{`"a,b", "c"`}, true, false},
		{"two field lines", []string{`"a"`, opaque}, true, true},
		{"star", []string{"*"}, true, true},
		{"star with a tag", []string{`*, "a"`}, false, false},
		{"unquoted", []string{"abc"}, false, false},
		{"unterminated", []string{`"abc`}, false, false},
		{"space in a tag", []string{`"a b"`}, false, false},
		{"empty value", []string{""}, true, false},
		{"only commas", []string{" , ,"}, true, false},
		{"only a space", []string{" "}, true, false},
		{"junk after a tag", []string{`"a" x`}, false, false},
		{"one bad line", []string{`"a"`, "abc"}, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Valid(tt.values); got != tt.valid {
				t.Errorf("Valid = %v, want %v", got, tt.valid)
			}

			if got := Match(tt.values, opaque); got != tt.match {
				t.Errorf("Match = %v, want %v", got, tt.match)
			}
		})
	}
}
