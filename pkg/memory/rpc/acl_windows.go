//go:build windows

package rpc

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// discoverySDDL is the protected DACL applied to the discovery file. The
// token inside it is the only access control on a TCP-loopback daemon, so
// the file must be readable by exactly: the owning user (whose memory it
// serves), SYSTEM and Administrators (so backup and admin tooling keep
// working, matching user-profile semantics). P marks the ACL protected — no
// ACEs inherited from the containing directory — which is the whole point:
// 0600 does not map to ACLs on Windows, so in a team-shared tree the file
// would otherwise inherit grants to every local user.
const discoverySDDLFormat = "D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)"

// secureDiscoveryFile tightens the discovery file after it lands on its
// final name; a failure aborts daemon startup rather than running with an
// unprotected token.
func secureDiscoveryFile(path string) error {
	return SecureFileOwnerOnly(path)
}

// SecureFileOwnerOnly replaces path's DACL with a protected, owner-only one:
// the current user, SYSTEM and Administrators get full access; nobody else,
// and nothing inherited from the containing directory. It exists because a
// 0600 mode is a POSIX promise only — on Windows os.WriteFile's mode does
// nothing, and a secret file would inherit whatever its directory grants,
// which in a team-shared tree includes every local user.
func SecureFileOwnerOnly(path string) error {
	userSID, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("rpc: resolve current user SID: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(discoverySDDLFormat, userSID))
	if err != nil {
		return fmt.Errorf("rpc: build owner-only DACL: %w", err)
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("rpc: read owner-only DACL: %w", err)
	}
	if defaulted || dacl == nil {
		return fmt.Errorf("rpc: owner-only DACL missing or marked defaulted")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("rpc: apply owner-only DACL: %w", err)
	}
	return nil
}

func currentUserSID() (string, error) {
	// GetCurrentProcessToken returns the pseudo-token: valid for queries,
	// and it must not be closed.
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}
