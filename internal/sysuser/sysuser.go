// Package sysuser is the MVP name for the workspace-account seam. Since the
// privilege split (plan M3.1) the implementation lives in internal/helper:
// the server no longer switches credentials itself, and every privileged
// action goes through the root helper's allowlisted API (contract §2.5).
//
// This package remains as a compatibility shim so code written against
// *sysuser.Account keeps compiling. Accounts are obtained from
// helper.Client.CreateWorkspaceUser; the MVP functions Ensure, Delete and
// Isolated are gone (use the server's helper client instead).
package sysuser

import "github.com/callmehalpha/Armageddon-/internal/helper"

// Account is a workspace's OS user. Command, Prepare, MkdirOwned and Chown
// keep their MVP shapes; see helper.Account for what changed.
type Account = helper.Account

// NameFor derives the OS user name for a workspace ID.
func NameFor(workspaceID string) string { return helper.UserName(workspaceID) }
