// Package workshop renders Steam Workshop mod surfaces: a steamcmd init
// container that downloads configured items into a game volume, plus env
// injection for images that self-manage workshop content.
package workshop

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

const (
	// steamcmdImage is the official steamcmd image used to download
	// workshop items. It exits after one invocation, so we drive a loop
	// in the container's command.
	steamcmdImage = "steamcmd/steamcmd:latest"

	// scratchMountPath is where steamcmd writes its working tree. We mount
	// an emptyDir here so downloads don't conflict with the game volume.
	scratchMountPath = "/steam"

	// scratchVolName is the emptyDir volume name mounted at scratchMountPath.
	scratchVolName = "steam-workshop-scratch"

	// downloadBase is the directory under /steam where workshop_download_item
	// writes its output. Format: /steam/steamapps/workshop/downloads/<appId>/<itemId>
	downloadBase = scratchMountPath + "/steamapps/workshop/downloads"
)

// InitContainer returns the steamcmd init container for the game's
// spec.mods.steamWorkshop config, or nil when not configured.
//
// The init container runs one steamcmd invocation per configured item,
// then symlinks each downloaded directory into the resolved MountPath
// (which must overlap a game volume for the artifacts to reach the game
// container). A scratch emptyDir holds the steamcmd working tree.
//
// Returns nil if workshop is not configured, if the MountPath cannot be
// resolved (no explicit MountPath and no volumes to derive from), or if
// no items are configured.
func InitContainer(game *gamesv1alpha1.HostedGame) *corev1.Container {
	cfg := workshopConfig(game)
	if cfg == nil {
		return nil
	}

	mountPath := resolveMountPath(game, cfg)
	if mountPath == "" {
		// Misconfiguration: workshop configured but no MountPath and no
		// volumes to default to. Cannot place items anywhere useful.
		return nil
	}
	if len(cfg.Items) == 0 {
		// Nothing to download. Skip the container entirely.
		return nil
	}

	return &corev1.Container{
		Name:            "steam-workshop",
		Image:           steamcmdImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args: []string{
			buildDownloadScript(cfg, mountPath),
		},
		Env: []corev1.EnvVar{
			{
				Name:  "STEAM_APP_ID",
				Value: strconv.FormatInt(cfg.AppID, 10),
			},
		},
		// steamcmd is heavy on CPU and memory during downloads. Default
		// to a generous envelope; users can override at the workload level
		// later if they need tighter limits.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      scratchVolName,
				MountPath: scratchMountPath,
			},
			{
				// Mount the resolved game volume at the requested MountPath
				// so symlinks survive init container exit. The name is the
				// spec.volume whose MountPath matches; the controller's
				// resources package attaches the same volume to the game
				// container by that name.
				Name:      resolveVolumeName(game, mountPath),
				MountPath: mountPath,
			},
		},
	}
}

// Env returns env vars injected into the game container (STEAM_WORKSHOP_ITEMS
// as a comma-separated item list, STEAM_WORKSHOP_APP_ID), or nil when
// workshop is not configured or has no items.
//
// These envs let game images that self-manage workshop content (e.g. some
// Zomboid / Valheim images) pick up the configured items without depending
// on the init container's symlinks.
func Env(game *gamesv1alpha1.HostedGame) []corev1.EnvVar {
	cfg := workshopConfig(game)
	if cfg == nil || len(cfg.Items) == 0 {
		return nil
	}

	items := make([]string, 0, len(cfg.Items))
	for _, id := range cfg.Items {
		items = append(items, strconv.FormatInt(id, 10))
	}

	return []corev1.EnvVar{
		{
			Name:  "STEAM_WORKSHOP_APP_ID",
			Value: strconv.FormatInt(cfg.AppID, 10),
		},
		{
			Name:  "STEAM_WORKSHOP_ITEMS",
			Value: strings.Join(items, ","),
		},
	}
}

// ScratchVolume is the emptyDir volume mounted at /steam inside the init
// container. The controller's resources package appends this to the pod's
// volumes when the workshop init container is present.
//
// Returns nil when workshop is not configured (so the controller can skip
// adding the volume).
func ScratchVolume(game *gamesv1alpha1.HostedGame) *corev1.Volume {
	if workshopConfig(game) == nil {
		return nil
	}
	return &corev1.Volume{
		Name: scratchVolName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

// workshopConfig returns the SteamWorkshopConfig pointer or nil.
func workshopConfig(game *gamesv1alpha1.HostedGame) *gamesv1alpha1.SteamWorkshopConfig {
	if game.Spec.Mods == nil || game.Spec.Mods.SteamWorkshop == nil {
		return nil
	}
	return game.Spec.Mods.SteamWorkshop
}

// resolveMountPath picks the mount path for downloaded items:
//  1. cfg.MountPath if set
//  2. otherwise the first spec.volume's MountPath (default mount target)
//  3. "" if nothing resolves
func resolveMountPath(game *gamesv1alpha1.HostedGame, cfg *gamesv1alpha1.SteamWorkshopConfig) string {
	if cfg.MountPath != "" {
		return cfg.MountPath
	}
	if len(game.Spec.Volumes) > 0 {
		return game.Spec.Volumes[0].MountPath
	}
	return ""
}

// resolveVolumeName returns the spec.volume name whose MountPath matches
// the given path. Falls back to the first volume if no exact match (so
// the container still mounts *somewhere* — logged via the symlink step
// failing loudly rather than silently).
func resolveVolumeName(game *gamesv1alpha1.HostedGame, mountPath string) string {
	for _, v := range game.Spec.Volumes {
		if v.MountPath == mountPath {
			return v.Name
		}
	}
	if len(game.Spec.Volumes) > 0 {
		return game.Spec.Volumes[0].Name
	}
	// Unreachable: resolveMountPath returned "" when there are no volumes,
	// and we return nil before getting here. Defensive fallback.
	return "game-data"
}

// buildDownloadScript assembles the shell script run by the steamcmd init
// container. One invocation per item (steamcmd's CLI is one-shot), then
// a symlink pass into the game volume's MountPath.
//
// Steamcmd downloads each item into
//
//	/steam/steamapps/workshop/downloads/<appId>/<itemId>
//
// as a directory whose contents are the mod files. We symlink each into
// "<mountPath>/<itemId>" so the game container sees a stable layout.
func buildDownloadScript(cfg *gamesv1alpha1.SteamWorkshopConfig, mountPath string) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("app=" + strconv.FormatInt(cfg.AppID, 10) + "\n")
	b.WriteString("dest=\"" + mountPath + "\"\n")
	b.WriteString("mkdir -p \"$dest\"\n")
	for _, item := range cfg.Items {
		id := strconv.FormatInt(item, 10)
		// Download the item. `+login anonymous` works for most dedicated-
		// server apps; games requiring a real login will fail here and the
		// init container exits non-zero, which surfaces as a pod create
		// error in the controller.
		b.WriteString("steamcmd +login anonymous +workshop_download_item $app " + id + " +quit\n")
		// Symlink the downloaded directory into the game volume. -f overwrites
		// any stale link from a previous pod start (emptyDir is per-pod so
		// this is defensive only).
		b.WriteString("ln -sfn \"" + downloadBase + "/" + id + "\" \"$dest/" + id + "\"\n")
	}
	b.WriteString("echo \"workshop: downloaded " + strconv.Itoa(len(cfg.Items)) + " item(s) into $dest\"\n")
	return b.String()
}
