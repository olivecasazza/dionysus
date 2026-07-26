package resources

import (
	"fmt"
	"path"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// ImportKind selects where a fetched artifact lands inside the game volume.
type ImportKind string

const (
	// ImportMod drops a .pak into the engine's mod directory.
	ImportMod ImportKind = "mod"
	// ImportWorld drops a save archive into the volume root for the game
	// (or an operator) to unpack.
	ImportWorld ImportKind = "world"
)

// importLimits are deliberately conservative. An import writes attacker-
// controlled bytes into a game server's volume, so everything that can be
// bounded is bounded.
const (
	maxImportBytes    = 2 << 30 // 2GiB — worlds get large; mods never should
	importJobTTL      = int32(3600)
	importJobDeadline = int64(1800)
)

// modSubdirs maps a game image family to the path (relative to the volume
// mount) where loose .pak mods belong. Only games we have actually verified
// are listed; anything else must specify destDir explicitly rather than
// having us guess and scatter files into a random directory.
var modSubdirs = map[string]string{
	"palworld": "Pal/Content/Paks/~mods",
}

// SanitizeFilename reduces an arbitrary user-supplied name to a single safe
// path element. Returns an error rather than silently rewriting, so a
// caller can reject the request outright.
//
// This is the primary guard against writing outside the intended directory:
// Discord filenames and URL basenames are both attacker-controlled.
func SanitizeFilename(name string) (string, error) {
	// REJECT rather than normalise. path.Base() would happily turn
	// "../../etc/passwd" into "passwd" and accept it — silently writing
	// somewhere the caller did not ask for. Failing closed means a
	// malformed name surfaces as an error instead of a surprise write.
	name = strings.TrimSpace(name)

	switch {
	case name == "", name == ".", name == "..":
		return "", fmt.Errorf("empty or non-file name")
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf("name must be a bare filename, not a path")
	case strings.HasPrefix(name, "-"):
		// Would be parsed as a flag by the fetch tooling.
		return "", fmt.Errorf("name may not start with '-'")
	case strings.ContainsAny(name, "\x00\n\r"):
		return "", fmt.Errorf("name contains control characters")
	case len(name) > 128:
		return "", fmt.Errorf("name longer than 128 characters")
	}
	return name, nil
}

// ValidateImport checks the kind/extension pairing. Extensions are an
// allowlist: a .sh or .so landing in a game volume is a remote-code-execution
// primitive, not a mod.
func ValidateImport(kind ImportKind, filename string) error {
	ext := strings.ToLower(path.Ext(filename))
	switch kind {
	case ImportMod:
		if ext != ".pak" && ext != ".ucas" && ext != ".utoc" {
			return fmt.Errorf("mod imports must be .pak/.ucas/.utoc, got %q", ext)
		}
	case ImportWorld:
		if ext != ".zip" && ext != ".tar" && ext != ".gz" && ext != ".tgz" && ext != ".sav" {
			return fmt.Errorf("world imports must be .zip/.tar/.gz/.tgz/.sav, got %q", ext)
		}
	default:
		return fmt.Errorf("unknown import kind %q", kind)
	}
	return nil
}

// ImportDestDir resolves the directory (relative to the volume mount) that a
// given kind lands in. Mods need a per-game path; worlds go to a staging dir
// so an import never overwrites a live save in place.
func ImportDestDir(game *gamesv1alpha1.HostedGame, kind ImportKind) (string, error) {
	switch kind {
	case ImportWorld:
		return "imports", nil
	case ImportMod:
		for family, dir := range modSubdirs {
			if strings.Contains(strings.ToLower(game.Spec.Image), family) {
				return dir, nil
			}
		}
		return "", fmt.Errorf(
			"no known mod directory for image %q; mod import is only wired for: %s",
			game.Spec.Image, strings.Join(keys(modSubdirs), ", "))
	default:
		return "", fmt.Errorf("unknown import kind %q", kind)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ImportJob renders a one-shot Job that fetches srcURL into the game's
// volume.
//
// nodeName pins the Job to the node currently running the game. The game
// volume is ReadWriteOnce: RWO permits many pods on ONE node, so co-locating
// is what lets an import run while the server is up. Passing "" schedules
// anywhere, which is only safe when the game is scaled to zero.
func ImportJob(
	game *gamesv1alpha1.HostedGame,
	kind ImportKind,
	srcURL, filename, nodeName string,
) (*batchv1.Job, error) {
	if len(game.Spec.Volumes) == 0 {
		return nil, fmt.Errorf("game %q has no volumes to import into", game.Name)
	}
	safeName, err := SanitizeFilename(filename)
	if err != nil {
		return nil, fmt.Errorf("filename: %w", err)
	}
	if err := ValidateImport(kind, safeName); err != nil {
		return nil, err
	}
	destDir, err := ImportDestDir(game, kind)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(srcURL, "https://") {
		// http:// would let a MITM choose the bytes we write to disk.
		return nil, fmt.Errorf("source URL must be https://")
	}

	vol := game.Spec.Volumes[0]
	claim := vol.ExistingClaim
	if claim == "" {
		claim = fmt.Sprintf("%s-%s", game.Name, vol.Name)
	}

	// Download to a temp path and rename only on success, so a truncated
	// transfer can never be mistaken for a valid mod by the engine.
	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("dest=%q", path.Join(vol.MountPath, destDir)),
		fmt.Sprintf("name=%q", safeName),
		`mkdir -p "$dest"`,
		`tmp="$dest/.$name.partial"`,
		`trap 'rm -f "$tmp"' EXIT`,
		fmt.Sprintf(
			`curl -fsSL --proto '=https' --max-filesize %d --max-time 900 -o "$tmp" %q`,
			maxImportBytes, srcURL),
		`test -s "$tmp"`,
		`mv -f "$tmp" "$dest/$name"`,
		`trap - EXIT`,
		`ls -lh "$dest/$name"`,
		`echo "import complete: $dest/$name"`,
	}, "\n")

	labels := commonLabels(game)
	labels["app.kubernetes.io/component"] = "import"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-import-%s-", game.Name, kind),
			Namespace:    game.Namespace,
			Labels:       labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0), // never silently retry a write
			TTLSecondsAfterFinished: int32Ptr(importJobTTL),
			ActiveDeadlineSeconds:   int64Ptr(importJobDeadline),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeName:      nodeName,
					Containers: []corev1.Container{{
						Name:    "import",
						Image:   "curlimages/curl:latest",
						Command: []string{"sh", "-c", script},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: vol.MountPath,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: claim,
							},
						},
					}},
				},
			},
		},
	}
	return job, nil
}

func int64Ptr(i int64) *int64 { return &i }
