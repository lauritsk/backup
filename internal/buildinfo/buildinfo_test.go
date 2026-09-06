package buildinfo

import "testing"

func TestInfoFormat(t *testing.T) {
	info := Info{
		Version:   "1.2.3",
		Revision:  "abc123",
		BuildTime: "2025-01-02T03:04:05Z",
		GoVersion: "go1.27.0",
	}

	got := info.Format("pimbackup")
	want := "pimbackup version=1.2.3 revision=abc123 build_time=2025-01-02T03:04:05Z go=go1.27.0"
	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}

func TestInfoFormatUsesFallbacks(t *testing.T) {
	got := (Info{}).Format("appbackup")
	want := "appbackup version=dev revision=unknown build_time=unknown go=unknown"
	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}
