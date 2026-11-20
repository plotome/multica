package forkupdate

import (
	"context"
	"errors"
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
		return packageValidationError{err}
	}
	err = VerifyArtifact(file, target.Artifact)
	file.Close()
	if err != nil {
		return packageValidationError{err}
	}
	return d.Driver.Install(ctx, target)
}

func (d verifiedPackageDriver) PrepareRecovery(ctx context.Context, target Target) (RecoveryPlan, error) {
	planner, ok := d.Driver.(RecoveryPlanner)
	if !ok {
		return RecoveryPlan{}, errors.New("release adapter has no recovery planner")
	}
	return planner.PrepareRecovery(ctx, target)
}

type packageValidationError struct{ error }
