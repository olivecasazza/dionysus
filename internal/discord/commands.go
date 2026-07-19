package discord

// ApplicationCommand is the JSON shape PUT to /applications/{app}/commands
// for slash-command registration. Reference:
// https://discord.com/developers/docs/interactions/application-commands#application-command-object
type ApplicationCommand struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Options     []ApplicationCommandOption `json:"options,omitempty"`
}

type ApplicationCommandOptionType int

const (
	OptionSubCommand ApplicationCommandOptionType = 1
	OptionString     ApplicationCommandOptionType = 3
)

type ApplicationCommandOption struct {
	Type        ApplicationCommandOptionType `json:"type"`
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Required    bool                         `json:"required"`
	Options     []ApplicationCommandOption   `json:"options,omitempty"`
}

// Commands returns every slash command the bot registers at startup.
// list / status / info are fully implemented in handler.go.
// start / stop / backup-now are stubs — they acknowledge but defer the
// action to a future controller-side wiring (idle lane already handles
// scale-to-zero; manual start needs a wake path; backup-now needs the
// controller to render the ad-hoc Job).
func Commands() []ApplicationCommand {
	return []ApplicationCommand{
		{
			Name:        "game",
			Description: "Manage Dionysus-hosted game servers",
			Options: []ApplicationCommandOption{
				{
					Type:        OptionSubCommand,
					Name:        "list",
					Description: "List all games visible to Discord (spec.discord.enabled=true)",
				},
				{
					Type:        OptionSubCommand,
					Name:        "status",
					Description: "Show phase, players, and address for one game",
					Options: []ApplicationCommandOption{
						{
							Type:        OptionString,
							Name:        "name",
							Description: "Game name (metadata.name of the HostedGame)",
							Required:    false,
						},
					},
				},
				{
					Type:        OptionSubCommand,
					Name:        "info",
					Description: "Show connection info and description for one game",
					Options: []ApplicationCommandOption{
						{
							Type:        OptionString,
							Name:        "name",
							Description: "Game name",
							Required:    true,
						},
					},
				},
				{
					Type:        OptionSubCommand,
					Name:        "start",
					Description: "Wake a stopped game (scale to 1)",
					Options: []ApplicationCommandOption{
						{
							Type:        OptionString,
							Name:        "name",
							Description: "Game name",
							Required:    true,
						},
					},
				},
				{
					Type:        OptionSubCommand,
					Name:        "stop",
					Description: "Stop a running game gracefully (scale to 0)",
					Options: []ApplicationCommandOption{
						{
							Type:        OptionString,
							Name:        "name",
							Description: "Game name",
							Required:    true,
						},
					},
				},
				{
					Type:        OptionSubCommand,
					Name:        "backup-now",
					Description: "Trigger an ad-hoc restic backup Job",
					Options: []ApplicationCommandOption{
						{
							Type:        OptionString,
							Name:        "name",
							Description: "Game name",
							Required:    true,
						},
					},
				},
			},
		},
	}
}
