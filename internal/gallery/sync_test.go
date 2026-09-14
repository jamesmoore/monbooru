package gallery

import (
	"slices"
	"testing"
)

func TestSortSyncFilesOldestFirst(t *testing.T) {
	files := []syncFileInfo{
		{path: "newest", mtimeNano: 30},
		{path: "same-b", mtimeNano: 20},
		{path: "oldest", mtimeNano: 10},
		{path: "same-a", mtimeNano: 20},
	}

	sortSyncFilesOldestFirst(files)

	got := make([]string, len(files))
	for i, file := range files {
		got[i] = file.path
	}
	want := []string{"oldest", "same-a", "same-b", "newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("ordered paths = %v, want %v", got, want)
	}
}
