package resources

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func backupGame() *gamesv1alpha1.HostedGame {
	return &gamesv1alpha1.HostedGame{
		ObjectMeta: metav1.ObjectMeta{Name: "valheim", Namespace: "games"},
		Spec: gamesv1alpha1.HostedGameSpec{
			Volumes: []gamesv1alpha1.GameVolume{
				{Name: "data", MountPath: "/data"},
				{Name: "config", MountPath: "/config"},
			},
			Backup: &gamesv1alpha1.BackupSpec{
				Driver:   gamesv1alpha1.BackupDriverRestic,
				Schedule: "0 10 * * *",
				S3: &gamesv1alpha1.S3Destination{
					Endpoint:             "storage.googleapis.com",
					Bucket:               "nixlab-game-backups",
					CredentialsSecretRef: corev1.LocalObjectReference{Name: "game-backup-key"},
				},
			},
		},
	}
}

func TestGameBackupCronJob(t *testing.T) {
	cj, err := GameBackupCronJob(backupGame())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if cj.Name != "valheim-backup" || cj.Spec.Schedule != "0 10 * * *" {
		t.Fatalf("name/schedule wrong: %s %s", cj.Name, cj.Spec.Schedule)
	}
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	if pod.Containers[0].Image != resticImage {
		t.Fatalf("image = %s", pod.Containers[0].Image)
	}
	// Both volumes mounted read-only, PVC name = <game>-<vol>.
	if len(pod.Volumes) != 2 || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "valheim-data" ||
		!pod.Volumes[0].PersistentVolumeClaim.ReadOnly {
		t.Fatalf("volume wrong: %+v", pod.Volumes)
	}
	if !pod.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatalf("mount not read-only")
	}
	// Repo points at the GCS interop endpoint; prefix defaults to game name.
	script := pod.Containers[0].Command[2]
	if !strings.Contains(script, "restic backup") || !strings.Contains(script, "/data /config") {
		t.Fatalf("script missing backup of both paths: %s", script)
	}
	var repo string
	for _, e := range pod.Containers[0].Env {
		if e.Name == "RESTIC_REPOSITORY" {
			repo = e.Value
		}
	}
	if repo != "s3:https://storage.googleapis.com/nixlab-game-backups/valheim" {
		t.Fatalf("repo = %s", repo)
	}
	// Contended RWO mount must fail fast, not hang.
	if cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds == nil {
		t.Fatalf("no activeDeadlineSeconds")
	}
}

func TestGameBackupNilWhenUnset(t *testing.T) {
	g := backupGame()
	g.Spec.Backup = nil
	cj, err := GameBackupCronJob(g)
	if err != nil || cj != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", cj, err)
	}
}
