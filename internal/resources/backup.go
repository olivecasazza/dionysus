package resources

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// resticImage runs the scheduled backup. restic gives snapshots, dedup, and
// retention pruning (forget --keep-*), which is exactly what BackupSpec.Retention
// models — a better fit than a plain rsync.
const resticImage = "restic/restic:0.17.3"

// GameBackupCronJob renders the scheduled backup CronJob for a HostedGame, or
// (nil, nil) when spec.backup is unset. Only the restic driver is implemented.
//
// It mounts the game's PVCs read-only and backs them up to an S3-compatible
// bucket (GCS via the interop endpoint storage.googleapis.com). Game PVCs are
// RWO, so a backup pod can only attach while the game is scaled to zero — the
// idle-scale-to-zero norm, and why the existing nightly backups work. A
// contended run (game up) can't attach the volume; activeDeadlineSeconds bounds
// it so the job fails fast and retries next schedule instead of hanging.
func GameBackupCronJob(game *gamesv1alpha1.HostedGame) (*batchv1.CronJob, error) {
	b := game.Spec.Backup
	if b == nil {
		return nil, nil
	}
	if b.Driver != gamesv1alpha1.BackupDriverRestic {
		return nil, fmt.Errorf("backup driver %q not implemented (only %q)", b.Driver, gamesv1alpha1.BackupDriverRestic)
	}
	if b.S3 == nil {
		return nil, fmt.Errorf("backup driver restic requires spec.backup.s3")
	}
	if len(game.Spec.Volumes) == 0 {
		return nil, fmt.Errorf("backup configured but the game declares no volumes to back up")
	}

	labels := commonLabels(game)
	labels[labelComp] = "backup"

	var mounts []corev1.VolumeMount
	var vols []corev1.Volume
	var paths []string
	for _, v := range game.Spec.Volumes {
		mounts = append(mounts, corev1.VolumeMount{Name: v.Name, MountPath: v.MountPath, ReadOnly: true})
		vols = append(vols, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: game.Name + "-" + v.Name,
					ReadOnly:  true,
				},
			},
		})
		paths = append(paths, v.MountPath)
	}

	prefix := b.S3.Prefix
	if prefix == "" {
		prefix = game.Name
	}
	endpoint := b.S3.Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	repo := fmt.Sprintf("s3:https://%s/%s/%s", endpoint, b.S3.Bucket, prefix)

	ret := b.Retention
	if ret == nil {
		ret = &gamesv1alpha1.RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 3}
	}
	script := strings.Join([]string{
		"set -eu",
		// Initialise the repo on first run (idempotent).
		"restic snapshots >/dev/null 2>&1 || restic init",
		fmt.Sprintf("restic backup --host %s %s", game.Name, strings.Join(paths, " ")),
		fmt.Sprintf("restic forget --prune --keep-last %d --keep-daily %d --keep-weekly %d --keep-monthly %d",
			ret.KeepLast, ret.KeepDaily, ret.KeepWeekly, ret.KeepMonthly),
	}, "\n")

	secret := b.S3.CredentialsSecretRef.Name
	env := []corev1.EnvVar{
		{Name: "RESTIC_REPOSITORY", Value: repo},
		secretEnv("RESTIC_PASSWORD", secret, "RESTIC_PASSWORD"),
		secretEnv("AWS_ACCESS_KEY_ID", secret, "AWS_ACCESS_KEY_ID"),
		secretEnv("AWS_SECRET_ACCESS_KEY", secret, "AWS_SECRET_ACCESS_KEY"),
	}
	if b.S3.Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: b.S3.Region})
	}

	deadline := int64(3600)
	backoff := int32(2)
	histLimit := int32(3)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      game.Name + "-backup",
			Namespace: game.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   b.Schedule,
			Suspend:                    &b.Suspend,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: &histLimit,
			FailedJobsHistoryLimit:     &histLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:          &backoff,
					ActiveDeadlineSeconds: &deadline,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{{
								Name:         "restic",
								Image:        resticImage,
								Command:      []string{"/bin/sh", "-c", script},
								Env:          env,
								VolumeMounts: mounts,
							}},
							Volumes: vols,
						},
					},
				},
			},
		},
	}, nil
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
	}
}
