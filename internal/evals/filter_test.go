package evals

import "testing"

func TestFilterCases(t *testing.T) {
	cases := []Case{{ID: "001-a"}, {ID: "026-b"}, {ID: "028-c"}}
	ids := func(cs []Case) (out []string) {
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	for _, tc := range []struct {
		only string
		want []string
	}{
		{"", []string{"001-a", "026-b", "028-c"}},
		{"028", []string{"028-c"}},
		{" 026 , 028-c ,", []string{"026-b", "028-c"}},
		{"099", nil},
	} {
		got := ids(FilterCases(cases, tc.only))
		if len(got) != len(tc.want) {
			t.Fatalf("FilterCases(%q) = %v, want %v", tc.only, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("FilterCases(%q) = %v, want %v", tc.only, got, tc.want)
			}
		}
	}
}
