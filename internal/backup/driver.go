// Package backup implements the pluggable backup driver surface. The
// controller asks the selected Driver for the desired per-game CronJob; the
// driver owns schedule, save-hook wiring, upload, and retention rendering.
package backup

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// Driver renders the desired backup CronJob for a game. Implementations must
// be idempotent: the controller diffs the returned object against the cluster.
type Driver interface {
	// Name identifies the driver ("restic").
	Name() string

	// DesiredCronJob returns the CronJob implementing spec.backup for the
	// game. The returned object must:
	//   - live in the game's namespace, named "<game>-backup"
	//   - carry labels app.kubernetes.io/name=<game>, app.kubernetes.io/component=backup
	//   - run the game's PreSaveCommand (if any) via the API before upload,
	//     skipping the save when the game pod is not running
	//   - mount every game volume with Backup=true read-only
	//   - set an ownerReference to the HostedGame
	DesiredCronJob(game *gamesv1alpha1.HostedGame) (*batchv1.CronJob, error)

	// BackupJob returns a one-shot Job that runs a single backup immediately
	// (Discord /game backup-now). Named "<game>-backup-<unix>" in the game's
	// namespace, same pod spec shape as the CronJob.
	BackupJob(game *gamesv1alpha1.HostedGame, name string) (*batchv1.Job, error)
}

// ForDriver returns the Driver for d.
func ForDriver(d gamesv1alpha1.BackupDriver) (Driver, error) {
	switch d {
	case gamesv1alpha1.BackupDriverRestic:
		return NewRestic(), nil
	default:
		return nil, fmt.Errorf("unsupported backup driver %q", d)
	}
}
