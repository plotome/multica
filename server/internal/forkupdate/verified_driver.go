package forkupdate

import (
	"context"
	"os"
)

// Preparation is cached while awaiting approval. Revalidate the exact bytes at
// the install boundary; a restarted worker must not trust a package_ready flag
// after local cache damage. Lifecycle adapters still validate their extracted
// release-tool manifests before switching services.
type verifiedPackageDriver struct {
	Driver
	path string
}

func (d verifiedPackageDriver) Install(ctx context.Context, target Target) error {
	file, err := os.Open(d.path)
	if err != nil {
		return err
	}
	err = VerifyArtifact(file, target.Artifact)
	file.Close()
	if err != nil {
		return err
	}
	return d.Driver.Install(ctx, target)
}
