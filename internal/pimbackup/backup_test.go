package pimbackup

import "testing"

func TestSelected(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		want    bool
	}{
		{name: "Archive/2024", include: []string{"*"}, want: true},
		{name: "Archive/2024", include: []string{"Archive/*"}, want: true},
		{name: "Archive/2024", include: []string{"Archive/*"}, exclude: []string{"*/2024"}, want: false},
		{name: "Inbox", include: []string{"Archive/*"}, want: false},
	}
	for _, test := range tests {
		if got := selected(test.name, test.include, test.exclude); got != test.want {
			t.Errorf("selected(%q, %q, %q) = %t, want %t", test.name, test.include, test.exclude, got, test.want)
		}
	}
}
