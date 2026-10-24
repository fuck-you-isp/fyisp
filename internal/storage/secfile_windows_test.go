package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestFileDACL: the file is created with the private DACL (the owner and
// SYSTEM, nothing inherited), and a copy with inherited permissions gets
// it back at open.
func TestFileDACL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := saveFile(path, &file{}); err != nil {
		t.Fatal(err)
	}
	checkDACL(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"targets":[],"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := Open(Options{Dir: dir}); s.Err() != nil {
		t.Fatal(s.Err())
	}
	checkDACL(t, path)
}

func checkDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String() // "D:P(A;;FA;;;OW)(A;;FA;;;SY)", flags may add AI
	i := strings.IndexByte(s, '(')
	if i < 0 || !strings.HasPrefix(s, "D:") || !strings.Contains(s[2:i], "P") || s[i:] != "(A;;FA;;;OW)(A;;FA;;;SY)" {
		t.Errorf("DACL %s, want %s", s, privateSDDL)
	}
}
