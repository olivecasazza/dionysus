// Package workshop renders Steam Workshop mod surfaces: a steamcmd init
// container that downloads configured items into a game volume, plus env
// injection for images that self-manage workshop content.
package workshop

import (
	corev1 "k8s.io/api/core/v1"

	gamesv1alpha1 "github.com/olivecasazza/game-operator/api/v1alpha1"
)

// InitContainer returns the steamcmd init container for the game's
// spec.mods.steamWorkshop config, or nil when not configured. The container
// must download each item into the resolved mount path and mount the same
// volume as the game container.
func InitContainer(game *gamesv1alpha1.HostedGame) *corev1.Container {
	if game.Spec.Mods == nil || game.Spec.Mods.SteamWorkshop == nil {
		return nil
	}
	// Implemented by the workshop lane.
	return nil
}

// Env returns env vars injected into the game container (STEAM_WORKSHOP_ITEMS
// as a comma-separated item list, STEAM_WORKSHOP_APP_ID), or nil when
// workshop is not configured.
func Env(game *gamesv1alpha1.HostedGame) []corev1.EnvVar {
	if game.Spec.Mods == nil || game.Spec.Mods.SteamWorkshop == nil {
		return nil
	}
	// Implemented by the workshop lane.
	return nil
}
