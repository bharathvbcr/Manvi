package devcouncil

import (
	"reflect"
	"testing"
)

func TestChangedFiles(t *testing.T) {
	tests := []struct {
		name      string
		diff      string
		wantFiles []string
	}{
		{
			name: "modified file",
			diff: `diff --git a/src/app.go b/src/app.go
--- a/src/app.go
+++ b/src/app.go
@@ -1,1 +1,2 @@
 package app
+var x = 1
`,
			wantFiles: []string{"src/app.go"},
		},
		{
			name: "added file",
			diff: `diff --git a/src/new.go b/src/new.go
new file mode 100644
--- /dev/null
+++ b/src/new.go
@@ -0,0 +1,1 @@
+package new
`,
			wantFiles: []string{"src/new.go"},
		},
		{
			name: "deleted file",
			diff: `diff --git a/src/old.go b/src/old.go
deleted file mode 100644
--- a/src/old.go
+++ /dev/null
@@ -1,1 +0,0 @@
-package old
`,
			wantFiles: []string{"src/old.go"},
		},
		{
			name: "pure rename without hunks",
			diff: `diff --git a/src/old.go b/src/renamed.go
similarity index 100%
rename from src/old.go
rename to src/renamed.go
`,
			wantFiles: []string{"src/renamed.go"},
		},
		{
			name: "rename with modifications",
			diff: `diff --git a/src/a.go b/src/b.go
rename from src/a.go
rename to src/b.go
--- a/src/a.go
+++ b/src/b.go
@@ -1,1 +1,2 @@
 package main
+// mod
`,
			wantFiles: []string{"src/b.go"},
		},
		{
			name: "multiple files mixed",
			diff: `diff --git a/src/mod.go b/src/mod.go
--- a/src/mod.go
+++ b/src/mod.go
@@ -1 +1 @@
-old
+new
diff --git a/src/del.go b/src/del.go
--- a/src/del.go
+++ /dev/null
@@ -1 +0,0 @@
-del
diff --git a/src/add.go b/src/add.go
--- /dev/null
+++ b/src/add.go
@@ -0,0 +1 @@
+add
`,
			wantFiles: []string{"src/mod.go", "src/del.go", "src/add.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := changedFiles(tc.diff)
			if err != nil {
				t.Fatalf("changedFiles returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.wantFiles) {
				t.Errorf("changedFiles() = %v, want %v", got, tc.wantFiles)
			}
		})
	}
}
