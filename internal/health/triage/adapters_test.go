package triage

import (
	"testing"

	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
)

func strptr(s string) *string { return &s }

func TestResolvePathForRescan(t *testing.T) {
	libDir := "/mnt/library"
	importDir := "/mnt/import"

	tests := []struct {
		name string
		cfg  *config.Config
		item *database.FileHealth
		want string
	}{
		{
			name: "library_path wins",
			cfg:  &config.Config{MountPath: "/mnt/altmount"},
			item: &database.FileHealth{FilePath: "tv/show.mkv", LibraryPath: strptr("/mnt/library/tv/show.mkv")},
			want: "/mnt/library/tv/show.mkv",
		},
		{
			name: "falls back to library_dir join",
			cfg:  &config.Config{MountPath: "/mnt/altmount", Health: config.HealthConfig{LibraryDir: &libDir}},
			item: &database.FileHealth{FilePath: "tv/show.mkv"},
			want: "/mnt/library/tv/show.mkv",
		},
		{
			name: "falls back to import_dir join",
			cfg: &config.Config{
				MountPath: "/mnt/altmount",
				Import:    config.ImportConfig{ImportDir: &importDir},
			},
			item: &database.FileHealth{FilePath: "tv/show.mkv"},
			want: "/mnt/import/tv/show.mkv",
		},
		{
			name: "falls back to mount_path join",
			cfg:  &config.Config{MountPath: "/mnt/altmount"},
			item: &database.FileHealth{FilePath: "tv/show.mkv"},
			want: "/mnt/altmount/tv/show.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePathForRescan(tt.cfg, tt.item)
			if got != tt.want {
				t.Errorf("resolvePathForRescan = %q; want %q", got, tt.want)
			}
		})
	}
}
