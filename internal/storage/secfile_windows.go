package storage

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateSDDL gives the file's owner and SYSTEM full access, and nobody
// else any (protected: nothing inherited from the directory).
const privateSDDL = "D:P(A;;FA;;;OW)(A;;FA;;;SY)"

// createPrivate creates path for writing, exclusively, with the private
// DACL from the start (no window where it is readable).
func createPrivate(path string) (*os.File, error) {
	sd, err := windows.SecurityDescriptorFromString(privateSDDL)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// openOwn opens path for reading, refuses a file another user owns, and
// puts the private DACL back on fyisp's own (a copy restored with
// inherited permissions). Owners allowed besides this user: SYSTEM, and
// Administrators, which own what an elevated process creates (they can
// take any file anyway).
func openOwn(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := ownFile(f, path); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func ownFile(f *os.File, path string) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !owner.Equals(u.User.Sid) && !owner.IsWellKnown(windows.WinLocalSystemSid) && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return fmt.Errorf("%s belongs to another user", path)
	}
	priv, err := windows.SecurityDescriptorFromString(privateSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := priv.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func syncDir(string) {} // NTFS renames are durable once the file is synced
