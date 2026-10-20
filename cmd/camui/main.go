// camui is the web app of the recorder as a stand-alone executable (the PowerShell system's
// dashboard, started by cam.ps1 -InstallUi). The code is internal/web; holocron runs the same
// code as its web role.
package main

import (
	"os"

	"holocron/internal/web"
)

func main() { os.Exit(web.Main(os.Args[1:])) }
