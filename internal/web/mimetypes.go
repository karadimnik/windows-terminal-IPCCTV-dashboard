package web

import "mime"

// The embedded UI must be served with the same Content-Type on every host. Go reads the operating
// system's table (the Windows registry says application/javascript for .js, a Linux mime.types
// file may differ), so the two types the page needs are pinned here.
func init() {
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
}
