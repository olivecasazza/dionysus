package discord

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/olivecasazza/dionysus/internal/resources"
)

// discordCDNHost is the only host an attachment may come from. A user can
// still pass an arbitrary https URL via the url option — that is the
// documented escape hatch for large files — but we never treat an
// attacker-supplied host as if Discord had vouched for it.
const discordCDNHost = "cdn.discordapp.com"

// handleUpload imports a mod or world archive into a game's volume.
//
// The work happens in a Job rather than inline for two reasons: Discord
// requires a response within ~3s, and the bot process has no business
// streaming gigabytes through its own memory. The response therefore
// reports that the import was *dispatched*, not that it succeeded.
func (b *Bot) handleUpload(ctx context.Context, name string, data *InteractionData) InteractionResponse {
	if name == "" {
		return errorResponse("game name is required")
	}
	opts := subOptions(data)

	kind := resources.ImportKind(opts["kind"])
	if kind != resources.ImportMod && kind != resources.ImportWorld {
		return errorResponse("kind must be 'mod' or 'world'")
	}

	srcURL, filename, err := resolveSource(data, opts)
	if err != nil {
		return errorResponse(err.Error())
	}

	game, err := b.findGame(ctx, name)
	if err != nil {
		return errorResponse(fmt.Sprintf("find game: %v", err))
	}

	// The game volume is ReadWriteOnce. RWO allows many pods on ONE node,
	// so an import can run alongside a live server only if it lands on the
	// same node. If the game is down there is nothing to co-locate with and
	// the scheduler may place the Job freely.
	nodeName, err := b.gameNodeName(ctx, game.Name, game.Namespace)
	if err != nil {
		return errorResponse(fmt.Sprintf("locate game pod: %v", err))
	}

	job, err := resources.ImportJob(game, kind, srcURL, filename, nodeName)
	if err != nil {
		return errorResponse(err.Error())
	}
	if err := b.k8s.Create(ctx, job); err != nil {
		return errorResponse(fmt.Sprintf("dispatch import Job: %v", err))
	}

	where := "scheduled freely (game is stopped)"
	if nodeName != "" {
		where = fmt.Sprintf("pinned to `%s` (game is running; RWO volume)", nodeName)
	}
	return plaintext(fmt.Sprintf(
		"📦 Importing **%s** into `%s` as a %s.\n"+
			"Job `%s` %s.\n"+
			"A mod only takes effect after the server restarts — use `/game stop` then `/game start`.",
		filename, game.Name, kind, job.Name, where))
}

// resolveSource picks the artifact source. An attachment and a url are
// mutually exclusive so there is never ambiguity about what got written.
func resolveSource(data *InteractionData, opts map[string]string) (srcURL, filename string, err error) {
	attachID := opts["file"]
	rawURL := strings.TrimSpace(opts["url"])

	switch {
	case attachID != "" && rawURL != "":
		return "", "", fmt.Errorf("pass either file or url, not both")
	case attachID != "":
		if data.Resolved == nil || data.Resolved.Attachments == nil {
			return "", "", fmt.Errorf("attachment was not resolved by Discord")
		}
		att, ok := data.Resolved.Attachments[attachID]
		if !ok {
			return "", "", fmt.Errorf("attachment %q not found in payload", attachID)
		}
		if !strings.HasPrefix(att.URL, "https://"+discordCDNHost+"/") {
			return "", "", fmt.Errorf("attachment URL is not on %s", discordCDNHost)
		}
		return att.URL, att.Filename, nil
	case rawURL != "":
		if !strings.HasPrefix(rawURL, "https://") {
			return "", "", fmt.Errorf("url must start with https://")
		}
		// Derive a filename from the path, ignoring any query string.
		trimmed := rawURL
		if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
			trimmed = trimmed[:i]
		}
		idx := strings.LastIndex(trimmed, "/")
		if idx < 0 || idx+1 >= len(trimmed) {
			return "", "", fmt.Errorf("cannot derive a filename from that url")
		}
		return rawURL, trimmed[idx+1:], nil
	default:
		return "", "", fmt.Errorf("provide either a file attachment or a url")
	}
}

// subOptions flattens the invoked subcommand's options into name→value.
func subOptions(data *InteractionData) map[string]string {
	out := map[string]string{}
	if data == nil || len(data.Options) == 0 {
		return out
	}
	for _, opt := range data.Options[0].Options {
		out[opt.Name] = opt.Value
	}
	return out
}

// gameNodeName returns the node running the game pod, or "" when the game
// has no running pod.
func (b *Bot) gameNodeName(ctx context.Context, name, namespace string) (string, error) {
	var pods corev1.PodList
	err := b.k8s.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":       "hostedgame",
			"app.kubernetes.io/instance":   name,
			"app.kubernetes.io/managed-by": "dionysus",
		})
	if err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			return p.Spec.NodeName, nil
		}
	}
	return "", nil
}
