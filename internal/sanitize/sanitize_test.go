package sanitize

import "testing"

func TestIdentifierSanitizerPolicy(t *testing.T) {
	long := make([]byte, 257)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "valid", in: "global/project.v1_model-2", want: "global/project.v1_model-2"},
		{name: "empty", in: "", want: "<invalid>"},
		{name: "control", in: "project\n", want: "<invalid>"},
		{name: "punctuation", in: "project:secret", want: "<invalid>"},
		{name: "invalid utf8", in: string([]byte{'p', 0xff}), want: "<invalid>"},
		{name: "overlong", in: string(long), want: "<invalid>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Identifier(tc.in); got != tc.want {
				t.Fatalf("Identifier(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
