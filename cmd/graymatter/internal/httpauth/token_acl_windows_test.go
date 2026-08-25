//go:build windows

package httpauth

import (
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Same regression class as the daemon's discovery file (see
// pkg/memory/rpc/sock_acl_windows_test.go): on Windows a 0600 mode grants
// nothing, so the persisted HTTP bearer token must end up behind a DACL that
// no group-wide SID can read.
func TestTokenFileDACL_IsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	token, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if !created {
		t.Fatal("expected a freshly minted token")
	}

	data, err := os.ReadFile(TokenFilePath(dir))
	if err != nil {
		t.Fatalf("owner cannot read token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != token {
		t.Error("token file does not round-trip")
	}

	sd, err := windows.GetNamedSecurityInfo(TokenFilePath(dir), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security descriptor: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatalf("no usable DACL on token file: v=%v", err)
	}

	const (
		accessAllowedACEType = 0
		accessDeniedACEType  = 1
	)
	base := unsafe.Add(unsafe.Pointer(dacl), unsafe.Sizeof(*dacl))
	for i := 0; i < int(dacl.AceCount); i++ {
		hdr := (*windows.ACE_HEADER)(base)
		switch hdr.AceType {
		case accessAllowedACEType, accessDeniedACEType:
			sid := (*windows.SID)(unsafe.Add(base, 8)).String()
			switch sid {
			case "S-1-5-32-545":
				t.Errorf("token DACL grants BUILTIN\\Users")
			case "S-1-5-11":
				t.Errorf("token DACL grants Authenticated Users")
			case "S-1-1-0":
				t.Errorf("token DACL grants Everyone")
			}
		}
		base = unsafe.Add(base, uintptr(hdr.AceSize))
	}
}
