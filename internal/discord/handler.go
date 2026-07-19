package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// Bot answers Discord interaction webhooks by reading HostedGame state
// from the cluster. It is stateless — every request is fully resolved
// against the API before responding synchronously.
type Bot struct {
	publicKey string        // hex Ed25519 public key for request verification
	k8s       client.Client // controller-runtime client
	namespace string        // optional restriction; empty = all namespaces
}

// NewBot constructs a Bot. publicKeyHex is the Discord app's hex-encoded
// Ed25519 public key (64 chars → 32 bytes).
func NewBot(publicKeyHex string, k8s client.Client, namespace string) *Bot {
	return &Bot{publicKey: publicKeyHex, k8s: k8s, namespace: namespace}
}

// ServeHTTP implements http.Handler for POST /interactions.
func (b *Bot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sig := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	body, err := readBody(r)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	ok, err := Verify(b.publicKey, sig, ts, body)
	if err != nil || !ok {
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var inter Interaction
	if err := json.Unmarshal(body, &inter); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Ping handshake — return type 1 pong.
	if inter.Type == InteractionPing {
		writeJSON(w, http.StatusOK, InteractionResponse{Type: CallbackPong})
		return
	}

	// Only application commands are routed here; anything else is a 400.
	if inter.Type != InteractionApplicationCommand || inter.Data == nil {
		writeJSON(w, http.StatusOK, errorResponse("unsupported interaction type"))
		return
	}

	// Top-level command is always /game with a subcommand. Resolve which.
	sub, name := parseSubcommand(inter.Data)
	resp := b.dispatch(r.Context(), sub, name)
	writeJSON(w, http.StatusOK, resp)
}

// dispatch routes a subcommand to its handler. Each handler reads
// HostedGames via the controller-runtime client and returns a response.
// Errors become ephemeral (only-invoker-visible) messages so a single
// bad request doesn't spam a channel.
func (b *Bot) dispatch(ctx context.Context, sub, name string) InteractionResponse {
	switch sub {
	case "list":
		return b.handleList(ctx)
	case "status":
		return b.handleStatus(ctx, name)
	case "info":
		return b.handleInfo(ctx, name)
	case "start":
		return stubResponse("start", "scale-to-1 path pending; idle lane currently owns replicas")
	case "stop":
		return stubResponse("stop", "graceful stop pending; idle lane currently owns replicas")
	case "backup-now":
		return stubResponse("backup-now", "ad-hoc Job creation pending controller wiring")
	default:
		return errorResponse(fmt.Sprintf("unknown subcommand %q", sub))
	}
}

// handleList enumerates Discord-enabled HostedGames across the cluster
// (or restricted to b.namespace). One embed per game; falls back to a
// plain-text message if no games match.
func (b *Bot) handleList(ctx context.Context) InteractionResponse {
	games, err := b.listGames(ctx)
	if err != nil {
		return errorResponse(fmt.Sprintf("list games: %v", err))
	}
	if len(games) == 0 {
		return plaintext("No games visible to Discord.")
	}

	sort.Slice(games, func(i, j int) bool {
		return games[i].Name < games[j].Name
	})

	embeds := make([]InteractionEmbed, 0, len(games))
	for _, g := range games {
		embeds = append(embeds, gameSummaryEmbed(&g))
	}
	// Discord caps embeds at 10 per message; if there are more, fall
	// back to a compact text list.
	if len(embeds) > 10 {
		var lines []string
		for _, g := range games {
			lines = append(lines, fmt.Sprintf("**%s** — %s, %d/%d players",
				g.Name, phaseEmoji(g.Status.Phase),
				playerOnline(&g), playerMax(&g)))
		}
		return plaintext(strings.Join(lines, "\n"))
	}
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{Embeds: embeds},
	}
}

// handleStatus shows one game's full status. Defaults to the first
// Discord-enabled game if name is empty.
func (b *Bot) handleStatus(ctx context.Context, name string) InteractionResponse {
	game, err := b.findGame(ctx, name)
	if err != nil {
		return errorResponse(err.Error())
	}
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{Embeds: []InteractionEmbed{gameDetailEmbed(game)}},
	}
}

// handleInfo shows a game's Discord metadata (description, publicHost,
// connectionHint) for joining.
func (b *Bot) handleInfo(ctx context.Context, name string) InteractionResponse {
	game, err := b.findGame(ctx, name)
	if err != nil {
		return errorResponse(err.Error())
	}
	if game.Spec.Discord == nil {
		return errorResponse(fmt.Sprintf("%s has no Discord metadata", game.Name))
	}
	d := game.Spec.Discord
	title := game.Name
	if d.Description != "" {
		title = fmt.Sprintf("%s — %s", game.Name, d.Description)
	}
	fields := []InteractionEmbedField{}
	if d.PublicHost != "" {
		fields = append(fields, InteractionEmbedField{Name: "Connect", Value: "`" + d.PublicHost + "`"})
	}
	if d.ConnectionHint != "" {
		fields = append(fields, InteractionEmbedField{Name: "How to join", Value: d.ConnectionHint})
	}
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{
			Embeds: []InteractionEmbed{{
				Title:  title,
				Fields: fields,
				Footer: &InteractionEmbedFooter{Text: "Dionysus game-operator"},
			}},
		},
	}
}

// ─── k8s accessors ─────────────────────────────────────────────────

// listGames returns Discord-enabled HostedGames. If b.namespace is
// non-empty, restricted to that namespace.
func (b *Bot) listGames(ctx context.Context) ([]gamesv1alpha1.HostedGame, error) {
	list := &gamesv1alpha1.HostedGameList{}
	opts := []client.ListOption{}
	if b.namespace != "" {
		opts = append(opts, client.InNamespace(b.namespace))
	}
	if err := b.k8s.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	out := make([]gamesv1alpha1.HostedGame, 0, len(list.Items))
	for _, g := range list.Items {
		// Defensive copy — iterating list.Items reuses the loop var's
		// backing memory, and we return pointers below.
		g := g
		if g.Spec.Discord != nil && g.Spec.Discord.Enabled {
			out = append(out, g)
		}
	}
	return out, nil
}

// findGame returns a single HostedGame by name. If name is empty,
// returns the first Discord-enabled game (so `/game status` with no
// arg is convenient in single-game clusters).
func (b *Bot) findGame(ctx context.Context, name string) (*gamesv1alpha1.HostedGame, error) {
	if name != "" {
		g := &gamesv1alpha1.HostedGame{}
		ns := b.namespace
		if ns == "" {
			ns = "apps" // sensible default; all nixlab games live in apps
		}
		if err := b.k8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, g); err != nil {
			return nil, fmt.Errorf("get %s: %w", name, err)
		}
		return g, nil
	}
	games, err := b.listGames(ctx)
	if err != nil {
		return nil, err
	}
	if len(games) == 0 {
		return nil, fmt.Errorf("no Discord-enabled games found")
	}
	return &games[0], nil
}

// ─── response helpers ──────────────────────────────────────────────

func errorResponse(msg string) InteractionResponse {
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{
			Content: "⚠️ " + msg,
			Flags:   FlagEphemeral,
		},
	}
}

func stubResponse(cmd, reason string) InteractionResponse {
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{
			Content: fmt.Sprintf("🚧 `/%s` is a stub: %s.", cmd, reason),
			Flags:   FlagEphemeral,
		},
	}
}

func plaintext(s string) InteractionResponse {
	return InteractionResponse{
		Type: CallbackChannelMessageWithSource,
		Data: &InteractionCallbackData{Content: s},
	}
}

// parseSubcommand extracts the subcommand name and the first string
// option value (commonly "name") from an InteractionData.
func parseSubcommand(data *InteractionData) (sub, firstName string) {
	if len(data.Options) == 0 {
		return "", ""
	}
	root := data.Options[0]
	sub = root.Name
	for _, opt := range root.Options {
		if opt.Value != "" {
			return sub, opt.Value
		}
	}
	return sub, ""
}

// ─── embed builders ────────────────────────────────────────────────

func gameSummaryEmbed(g *gamesv1alpha1.HostedGame) InteractionEmbed {
	title := g.Name
	if g.Spec.DisplayName != "" {
		title = g.Spec.DisplayName
	}
	desc := string(g.Status.Phase)
	if g.Spec.Discord != nil && g.Spec.Discord.Description != "" {
		desc = g.Spec.Discord.Description
	}
	return InteractionEmbed{
		Title:       title,
		Description: desc,
		Color:       phaseColor(g.Status.Phase),
		Fields: []InteractionEmbedField{
			{Name: "Phase", Value: phaseEmoji(g.Status.Phase), Inline: true},
			{Name: "Players", Value: fmt.Sprintf("%d / %d", playerOnline(g), playerMax(g)), Inline: true},
			{Name: "Address", Value: addressOrNone(g.Status.Address), Inline: true},
		},
	}
}

func gameDetailEmbed(g *gamesv1alpha1.HostedGame) InteractionEmbed {
	fields := []InteractionEmbedField{
		{Name: "Phase", Value: string(g.Status.Phase), Inline: true},
		{Name: "Players", Value: fmt.Sprintf("%d / %d", playerOnline(g), playerMax(g)), Inline: true},
		{Name: "Address", Value: addressOrNone(g.Status.Address), Inline: true},
	}
	if g.Status.Backup != nil && g.Status.Backup.LastResult != "" {
		fields = append(fields, InteractionEmbedField{
			Name:  "Last backup",
			Value: fmt.Sprintf("%s — %s", g.Status.Backup.LastResult, g.Status.Backup.Message),
		})
	}
	return InteractionEmbed{
		Title:  g.Name,
		Color:  phaseColor(g.Status.Phase),
		Fields: fields,
		Footer: &InteractionEmbedFooter{Text: "Dionysus game-operator"},
	}
}

// phaseColor maps a GamePhase to a Discord embed color (decimal RGB).
// Discord caps embed color at 0xFFFFFF.
func phaseColor(p gamesv1alpha1.GamePhase) int {
	switch p {
	case gamesv1alpha1.PhaseRunning:
		return 0x57F287 // green
	case gamesv1alpha1.PhaseStarting, gamesv1alpha1.PhaseStopping:
		return 0xFEE75C // yellow
	case gamesv1alpha1.PhaseStopped:
		return 0x95A0A6 // gray
	case gamesv1alpha1.PhaseFailed:
		return 0xED4245 // red
	default:
		return 0x95A0A6
	}
}

func phaseEmoji(p gamesv1alpha1.GamePhase) string {
	switch p {
	case gamesv1alpha1.PhaseRunning:
		return "🟢 Running"
	case gamesv1alpha1.PhaseStarting:
		return "🟡 Starting"
	case gamesv1alpha1.PhaseStopping:
		return "🟠 Stopping"
	case gamesv1alpha1.PhaseStopped:
		return "⚪ Stopped"
	case gamesv1alpha1.PhaseFailed:
		return "🔴 Failed"
	default:
		return string(p)
	}
}

func playerOnline(g *gamesv1alpha1.HostedGame) int32 {
	if g.Status.Players == nil {
		return 0
	}
	return g.Status.Players.Online
}

func playerMax(g *gamesv1alpha1.HostedGame) int32 {
	if g.Status.Players == nil {
		return 0
	}
	return g.Status.Players.Max
}

func addressOrNone(addr string) string {
	if addr == "" {
		return "—"
	}
	return "`" + addr + "`"
}

// ─── HTTP plumbing ─────────────────────────────────────────────────

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return readAll(r.Body)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Can't use controller-runtime log here without a request context;
		// best-effort stderr for visibility in pod logs.
		fmt.Fprintf(os.Stderr, "discord: write interaction response: %v\n", err)
	}
}
