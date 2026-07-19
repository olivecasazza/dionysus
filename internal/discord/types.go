package discord

// InteractionType matches Discord's interaction kind constants.
// Reference: https://discord.com/developers/docs/interactions/receiving-and-responding#interaction-object
type InteractionType int

const (
	// InteractionPing is the initial verification handshake Discord
	// sends when the interactions URL is first registered.
	InteractionPing InteractionType = 1
	// InteractionApplicationCommand is a slash command invocation.
	InteractionApplicationCommand InteractionType = 2
)

// InteractionCallbackType controls how the response is delivered.
type InteractionCallbackType int

const (
	// CallbackPong answers InteractionPing.
	CallbackPong InteractionCallbackType = 1
	// CallbackChannelMessageWithSource replies with a message,
	// synchronously.
	CallbackChannelMessageWithSource InteractionCallbackType = 4
	// CallbackDeferredChannelMessageWithSource acknowledges and lets
	// us follow up later (we don't use this yet — every command
	// resolves synchronously).
	CallbackDeferredChannelMessageWithSource InteractionCallbackType = 5
)

// Interaction is the inbound webhook payload. Only fields we actually
// inspect are typed; the rest ride along as raw JSON for forward
// compatibility.
type Interaction struct {
	Type      InteractionType    `json:"type"`
	Data      *InteractionData   `json:"data,omitempty"`
	Member    *InteractionMember `json:"member,omitempty"`
	GuildID   string             `json:"guild_id,omitempty"`
	ChannelID string             `json:"channel_id,omitempty"`
}

// InteractionData is the parsed slash command invocation.
type InteractionData struct {
	Name    string              `json:"name"`
	Options []InteractionOption `json:"options,omitempty"`
}

// InteractionOption is one positional or named argument.
type InteractionOption struct {
	Name    string              `json:"name"`
	Value   string              `json:"value,omitempty"`
	Options []InteractionOption `json:"options,omitempty"`
}

// InteractionMember is the invoking Discord user (truncated to the
// fields we need for permission decisions later).
type InteractionMember struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

// InteractionResponse is what we POST back synchronously.
type InteractionResponse struct {
	Type InteractionCallbackType  `json:"type"`
	Data *InteractionCallbackData `json:"data,omitempty"`
}

// InteractionCallbackData carries the message body, embeds, and flags.
type InteractionCallbackData struct {
	Content string             `json:"content,omitempty"`
	Embeds  []InteractionEmbed `json:"embeds,omitempty"`
	Flags   int                `json:"flags,omitempty"` // 64 = Ephemeral
}

// InteractionEmbed is the Discord rich-embed object. Only the fields
// we actually populate.
type InteractionEmbed struct {
	Title       string                  `json:"title,omitempty"`
	Description string                  `json:"description,omitempty"`
	URL         string                  `json:"url,omitempty"`
	Color       int                     `json:"color,omitempty"`
	Fields      []InteractionEmbedField `json:"fields,omitempty"`
	Footer      *InteractionEmbedFooter `json:"footer,omitempty"`
}

type InteractionEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type InteractionEmbedFooter struct {
	Text string `json:"text"`
}

// FlagEphemeral marks a response as visible only to the invoker.
const FlagEphemeral = 64
