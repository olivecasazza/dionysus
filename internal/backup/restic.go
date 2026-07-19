// Package backup implements the pluggable backup driver surface. This
// file is the restic driver; the Driver interface and ForDriver switch
// live in driver.go.
package backup

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/scheme"
)

// resticImage is pinned; the operator deliberately does not chase :latest
// for a tool that writes to a remote repository it cannot easily roll back.
const resticImage = "restic/restic:0.16.4"

// resticDriver renders backup CronJobs / Jobs that run restic against an
// S3-compatible bucket. One driver instance is reusable across games
// (it holds no per-game state).
type resticDriver struct{}

// NewRestic returns a restic-backed Driver.
func NewRestic() Driver { return &resticDriver{} }

func (r *resticDriver) Name() string { return "restic" }

// DesiredCronJob returns the per-game CronJob that runs scheduled
// restic backups. See the Driver interface (driver.go) for the contract.
//
// Gap: PreSaveCommand (world save before backup) cannot be invoked from
// a standalone CronJob pod — that pod cannot exec into the game pod
// directly without significant RBAC + plumbing. The controller is the
// right place to invoke pre-save via the API before triggering the
// backup Job; that wiring is a controller-lane follow-up. For now the
// rendered CronJob runs `restic backup` cold, which is safe for games
// whose on-disk format tolerates a crash-consistent snapshot (most
// Minecraft worlds, Zomboid's sqlite-with-WAL, etc.) and explicitly
// unsafe for the rest. Document this loudly in operator docs.
func (r *resticDriver) DesiredCronJob(game *gamesv1alpha1.HostedGame) (*batchv1.CronJob, error) {
	if game.Spec.Backup == nil {
		return nil, fmt.Errorf("restic: spec.backup is nil for %s/%s", game.Namespace, game.Name)
	}
	if game.Spec.Backup.Driver != gamesv1alpha1.BackupDriverRestic {
		return nil, fmt.Errorf("restic: driver mismatch (spec wants %q)", game.Spec.Backup.Driver)
	}
	if game.Spec.Backup.S3 == nil {
		return nil, fmt.Errorf("restic: spec.backup.s3 is required for driver=restic")
	}

	template, err := r.podTemplate(game)
	if err != nil {
		return nil, err
	}

	cj := &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "CronJob",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      game.Name + "-backup",
			Namespace: game.Namespace,
			Labels:    backupLabels(game),
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   game.Spec.Backup.Schedule,
			TimeZone:                   nil, // cluster-local time; user overrides via TZ env if needed
			StartingDeadlineSeconds:    nil,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			Suspend:                    boolPtr(game.Spec.Backup.Suspend),
			SuccessfulJobsHistoryLimit: int32Ptr(1),
			FailedJobsHistoryLimit:     int32Ptr(3),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: backupLabels(game),
				},
				Spec: batchv1.JobSpec{
					Parallelism:           int32Ptr(1),
					Completions:           int32Ptr(1),
					BackoffLimit:          int32Ptr(2),
					ActiveDeadlineSeconds: nil,
					Template:              template,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(game, cj, scheme.Scheme); err != nil {
		return nil, fmt.Errorf("set owner ref: %w", err)
	}
	return cj, nil
}

// BackupJob returns a one-shot Job that runs a single backup immediately.
// The controller (or Discord /game backup-now command) supplies a unique
// name (typically "<game>-backup-<unix>"); we use it as-is.
func (r *resticDriver) BackupJob(game *gamesv1alpha1.HostedGame, name string) (*batchv1.Job, error) {
	if game.Spec.Backup == nil {
		return nil, fmt.Errorf("restic: spec.backup is nil for %s/%s", game.Namespace, game.Name)
	}
	if game.Spec.Backup.S3 == nil {
		return nil, fmt.Errorf("restic: spec.backup.s3 is required for driver=restic")
	}

	template, err := r.podTemplate(game)
	if err != nil {
		return nil, err
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: game.Namespace,
			Labels:    backupLabels(game),
		},
		Spec: batchv1.JobSpec{
			Parallelism:  int32Ptr(1),
			Completions:  int32Ptr(1),
			BackoffLimit: int32Ptr(1),
			Template:     template,
		},
	}

	if err := controllerutil.SetControllerReference(game, job, scheme.Scheme); err != nil {
		return nil, fmt.Errorf("set owner ref: %w", err)
	}
	return job, nil
}

// podTemplate renders the Pod spec shared by both the CronJob and the
// ad-hoc backup Job. Inputs are validated up-front so callers get a
// single clear error rather than a half-rendered object.
func (r *resticDriver) podTemplate(game *gamesv1alpha1.HostedGame) (corev1.PodTemplateSpec, error) {
	if len(game.Spec.Volumes) == 0 {
		return corev1.PodTemplateSpec{}, fmt.Errorf("restic: backup requested but %s/%s has no volumes", game.Namespace, game.Name)
	}

	mounts, vols, paths := backupVolumes(game)
	if len(paths) == 0 {
		return corev1.PodTemplateSpec{}, fmt.Errorf("restic: no volumes with Backup=true on %s/%s", game.Namespace, game.Name)
	}

	s3 := game.Spec.Backup.S3
	repo := s3RepoURL(s3)

	// Restic reads AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / RESTIC_PASSWORD
	// from the env. We use envFrom so the same Secret can carry additional
	// S3 knobs (AWS_DEFAULT_REGION, RESTIC_PACK_SIZE, etc.) without code
	// changes here.
	envFrom := []corev1.EnvFromSource{
		{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: s3.CredentialsSecretRef,
			},
		},
	}

	// RESTIC_REPOSITORY must be set explicitly because it's per-game; we
	// don't want it inherited from the Secret (which would collide for
	// games sharing one credentials Secret).
	env := []corev1.EnvVar{
		{Name: "RESTIC_REPOSITORY", Value: repo},
	}

	container := corev1.Container{
		Name:            "restic",
		Image:           resticImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{backupScript(game, paths)},
		Env:             env,
		EnvFrom:         envFrom,
		VolumeMounts:    mounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: backupLabels(game),
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers:    []corev1.Container{container},
			Volumes:       vols,
		},
	}, nil
}

// backupVolumes returns the volumeMounts, pod volumes, and absolute
// mount paths for every game volume with Backup=true. Volumes marked
// Backup=false (e.g. cache volumes, re-downloadable server binaries)
// are excluded so we don't waste bandwidth backing up transient state.
//
// existingClaim is honored; otherwise the operator-managed
// "<game>-<volume>" PVC name is used.
func backupVolumes(game *gamesv1alpha1.HostedGame) (
	mounts []corev1.VolumeMount,
	vols []corev1.Volume,
	paths []string,
) {
	for _, v := range game.Spec.Volumes {
		if !v.Backup {
			continue
		}
		claim := v.ExistingClaim
		if claim == "" {
			claim = game.Name + "-" + v.Name
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name:      v.Name,
			MountPath: v.MountPath,
			ReadOnly:  true,
		})
		vols = append(vols, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
					ReadOnly:  true,
				},
			},
		})
		paths = append(paths, v.MountPath)
	}
	return mounts, vols, paths
}

// s3RepoURL renders the restic repository URL: s3:endpoint/bucket/prefix.
// endpoint is empty for AWS defaults; the leading "s3:" is restic's S3
// backend marker. Prefix defaults to the game name if not set.
func s3RepoURL(s3 *gamesv1alpha1.S3Destination) string {
	prefix := s3.Prefix
	// Prefix is resolved against the game name at call time by the
	// controller (it can't be done here without the game). For the in-tree
	// call path, s3RepoURL is invoked from podTemplate which has the game;
	// we leave prefix verbatim and let the controller substitute later if
	// it wants the game name. For now: caller is responsible for setting
	// spec.backup.s3.prefix explicitly.
	endpoint := s3.Endpoint
	if endpoint == "" {
		// restic accepts "s3:s3.amazonaws.com/bucket" for AWS default; the
		// caller may omit endpoint only for AWS regions. Document that
		// GCS / B2 / MinIO users must set endpoint.
		endpoint = "s3.amazonaws.com"
	}
	return fmt.Sprintf("s3:%s/%s/%s", endpoint, s3.Bucket, prefix)
}

// backupScript assembles the restic backup + forget shell snippet. The
// script is intentionally explicit (no variables for paths) so logs are
// self-explanatory in `kubectl logs`.
func backupScript(game *gamesv1alpha1.HostedGame, paths []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("echo \"[restic] starting backup for " + game.Namespace + "/" + game.Name + "\"\n")

	if game.Spec.Lifecycle != nil && len(game.Spec.Lifecycle.PreSaveCommand) > 0 {
		// Documented gap: PreSaveCommand is not invoked here. The CronJob
		// pod has no path to exec the game pod. Log it so operators see
		// that a save was configured but skipped.
		b.WriteString("echo \"[restic] WARNING: spec.lifecycle.preSaveCommand is set but not invoked from the backup pod; pre-save must be wired in the controller before upload\"\n")
	}

	b.WriteString("restic snapshots >/dev/null 2>&1 || restic init\n")
	for _, p := range paths {
		b.WriteString("restic backup \"" + p + "\"\n")
	}

	// Forget + prune. Keep-defaults come from spec.backup.retention; the
	// zero value is handled below by skipping the flag entirely (restic's
	// own defaults then apply, which are conservative).
	r := game.Spec.Backup.Retention
	if r != nil {
		var forget []string
		if r.KeepLast > 0 {
			forget = append(forget, fmt.Sprintf("--keep-last=%d", r.KeepLast))
		}
		if r.KeepDaily > 0 {
			forget = append(forget, fmt.Sprintf("--keep-daily=%d", r.KeepDaily))
		}
		if r.KeepWeekly > 0 {
			forget = append(forget, fmt.Sprintf("--keep-weekly=%d", r.KeepWeekly))
		}
		if r.KeepMonthly > 0 {
			forget = append(forget, fmt.Sprintf("--keep-monthly=%d", r.KeepMonthly))
		}
		if len(forget) > 0 {
			b.WriteString("restic forget --prune " + strings.Join(forget, " ") + "\n")
		}
	}

	b.WriteString("echo \"[restic] backup complete\"\n")
	return b.String()
}

// backupLabels returns the labels applied to every backup object.
func backupLabels(game *gamesv1alpha1.HostedGame) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       game.Name,
		"app.kubernetes.io/component":  "backup",
		"app.kubernetes.io/managed-by": "dionysus",
	}
}

func int32Ptr(i int32) *int32 { return &i }

func boolPtr(b bool) *bool { return &b }
