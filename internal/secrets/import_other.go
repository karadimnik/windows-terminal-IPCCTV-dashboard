//go:build !windows

package secrets

import "context"

// ImportLegacyCameraCreds converts camera_creds.xml, a DPAPI file that only Windows can open.
func ImportLegacyCameraCreds(ctx context.Context, path string, dst Provider) error {
	return ErrUnsupported
}
