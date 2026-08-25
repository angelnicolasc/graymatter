//go:build windows

package rpc

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// aceSIDs walks a raw ACL and returns the string form of every grantee SID.
// x/sys exposes no GetAce binding, so this follows the documented on-disk
// layout: each ACE is a variable-length block whose first field is an
// ACE_HEADER carrying its own size; for allow/deny ACEs the SID starts right
// after the fixed part (header + mask = 8 bytes).
func aceSIDs(acl *windows.ACL) ([]string, error) {
	var out []string
	const (
		accessAllowedACEType = 0
		accessDeniedACEType  = 1
	)
	base := unsafe.Add(unsafe.Pointer(acl), unsafe.Sizeof(*acl))
	for i := 0; i < int(acl.AceCount); i++ {
		if uintptr(base)%4 != 0 {
			return nil, errors.New("ace pointer not 4-byte aligned")
		}
		hdr := (*windows.ACE_HEADER)(base)
		switch hdr.AceType {
		case accessAllowedACEType, accessDeniedACEType:
			sid := (*windows.SID)(unsafe.Add(base, 8))
			out = append(out, sid.String())
		}
		base = unsafe.Add(base, uintptr(hdr.AceSize))
	}
	return out, nil
}

// The discovery file is the daemon's only access control on Windows: the
// listener binds TCP loopback, which every local process can reach, so
// whoever reads the token can drive the store. A 0600 mode does NOT map to
// ACLs there — the file inherits whatever its containing directory grants,
// which in a team-shared tree means every local user. The regression this
// pins: writeDiscovery must leave behind a DACL that grants nothing to
// BUILTIN\Users (S-1-5-32-545), Authenticated Users (S-1-5-11) or Everyone,
// and whose every ACE belongs to the owning user, SYSTEM or Administrators.
func TestDiscoveryFileDACL_IsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	const addr = "tcp://127.0.0.1:1"
	token := "cafebabe" + strings.Repeat("0", 56)
	if err := writeDiscovery(dir, addr, token); err != nil {
		t.Fatalf("writeDiscovery: %v", err)
	}
	path := DiscoveryFilePath(dir)

	// The legitimate owner path keeps working: same process can read.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("owner cannot read discovery file: %v", err)
	}
	if !strings.Contains(string(data), token) {
		t.Error("token missing from discovery file")
	}

	userSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("resolve current user SID: %v", err)
	}
	allowed := map[string]bool{
		userSID:        true, // the owner
		"S-1-5-18":     true, // SYSTEM — backup/admin tooling semantics
		"S-1-5-32-544": true, // Administrators
	}
	forbidden := map[string]string{
		"S-1-5-32-545": "BUILTIN\\Users",
		"S-1-5-11":     "Authenticated Users",
		"S-1-1-0":      "Everyone",
	}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security descriptor: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read DACL: v=%v", err)
	}
	if dacl.AceCount == 0 {
		t.Fatal("DACL carries no ACEs")
	}
	sids, err := aceSIDs(dacl)
	if err != nil {
		t.Fatalf("walk DACL: %v", err)
	}
	for _, sid := range sids {
		if reason, bad := forbidden[sid]; bad {
			t.Errorf("discovery DACL grants %s (%s)", sid, reason)
		} else if !allowed[sid] {
			t.Errorf("unexpected ACE for %q in discovery DACL", sid)
		}
	}
}
