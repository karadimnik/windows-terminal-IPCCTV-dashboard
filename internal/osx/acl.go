package osx

// ownerOnlyICACLS lists the icacls argument lists (after the path) that make path owner-only for the
// user with the given SID: /reset first drops every explicit entry (an Everyone grant, say), then
// the inherited ones are removed and only that user is granted.
func ownerOnlyICACLS(path, sid string, isDir bool) [][]string {
	grant := "*" + sid + ":F"
	if isDir {
		grant = "*" + sid + ":(OI)(CI)F"
	}
	return [][]string{
		{path, "/reset"},
		{path, "/inheritance:r", "/grant:r", grant},
	}
}
