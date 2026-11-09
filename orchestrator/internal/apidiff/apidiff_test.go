package apidiff

import (
	"strings"
	"testing"
)

const diff = `diff --git a/pkg/store/users.go b/pkg/store/users.go
index 1111111..2222222 100644
--- a/pkg/store/users.go
+++ b/pkg/store/users.go
@@ -10,7 +10,8 @@ package store
-func FindUser(db *sql.DB, id int) (*User, error) {
+func FindUser(db *sql.DB, name string) (*User, error) {
+	return nil, nil
 }
-func (s *Store) Close() error { return nil }
+type Options struct{ Timeout time.Duration }
 func helper() {}
+func internalOnly() {}
diff --git a/pkg/store/users_test.go b/pkg/store/users_test.go
--- a/pkg/store/users_test.go
+++ b/pkg/store/users_test.go
@@ -1,1 +1,2 @@
+func TestNew(t *testing.T) {}
diff --git a/web/src/api.ts b/web/src/api.ts
--- a/web/src/api.ts
+++ b/web/src/api.ts
@@ -1,3 +1,3 @@
-export function getUser(id: number): User {
+export async function getUser(id: string): Promise<User> {
 export const VERSION = "1";
+export { a, b }
diff --git a/lib/calc.py b/lib/calc.py
--- a/lib/calc.py
+++ b/lib/calc.py
@@ -1,4 +1,4 @@
-def add(a, b):
+def add(a, b, c=0):
 def _private():
+class Calculator:
diff --git a/img.png b/img.png
Binary files a/img.png and b/img.png differ
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package x
-const Limit = 3
`

func TestParse(t *testing.T) {
	fs := Parse(diff)
	if len(fs) != 6 {
		t.Fatalf("%d files", len(fs))
	}
	if fs[0].Path() != "pkg/store/users.go" || len(fs[0].Hunks) != 1 || fs[0].Hunks[0].OldStart != 10 || fs[0].Hunks[0].Added() != 4 || fs[0].Hunks[0].Removed() != 2 {
		t.Errorf("file 0: %+v", fs[0])
	}
	if !fs[4].Binary || fs[5].Path() != "old.go" || fs[5].NewPath != "/dev/null" {
		t.Errorf("binary / deleted: %+v %+v", fs[4], fs[5])
	}
}

func TestExtract(t *testing.T) {
	got := Render(Extract(Parse(diff)))
	for _, want := range []string{
		"changed pkg/store/users.go FindUser (go)",
		"removed pkg/store/users.go Store.Close (go)",
		"added pkg/store/users.go Options (go)",
		"changed web/src/api.ts getUser (ts)",
		"added web/src/api.ts {a, b} (ts)",
		"changed lib/calc.py add (py)",
		"added lib/calc.py Calculator (py)",
		"removed old.go Limit (go)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, not := range []string{"helper", "internalOnly", "TestNew", "_private", "VERSION"} {
		if strings.Contains(got, not) {
			t.Errorf("%s reported:\n%s", not, got)
		}
	}
	if Render(nil) != "(none detected)" {
		t.Error("empty render")
	}
}
