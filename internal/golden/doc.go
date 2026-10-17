// Package golden holds the tests that load the golden files under
// testdata/golden (generated from the PowerShell product by
// tests/holocron/regen-goldens.ps1) and check they parse and are not empty.
// Later packages (ffmpeg, relay, archive, clock) compare their own output to
// the same files.
package golden
