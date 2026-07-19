package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GamePhase describes the lifecycle phase of a HostedGame.
// +kubebuilder:validation:Enum=Stopped;Starting;Running;Stopping;Failed
type GamePhase string

const (
	PhaseStopped  GamePhase = "Stopped"
	PhaseStarting GamePhase = "Starting"
	PhaseRunning  GamePhase = "Running"
	PhaseStopping GamePhase = "Stopping"
	PhaseFailed   GamePhase = "Failed"
)

// QueryType identifies the game-query protocol used for player counts and
// liveness. A2S covers most Steam dedicated servers (Valheim, Zomboid, Rust,
// CS). Minecraft uses the Server List Ping protocol.
// +kubebuilder:validation:Enum=A2S;Minecraft
type QueryType string

const (
	QueryTypeA2S       QueryType = "A2S"
	QueryTypeMinecraft QueryType = "Minecraft"
)

// BackupDriver selects the backup implementation.
// +kubebuilder:validation:Enum=restic;longhorn
type BackupDriver string

const (
	BackupDriverRestic   BackupDriver = "restic"
	BackupDriverLonghorn BackupDriver = "longhorn" // reserved; not yet implemented
)

// GamePort exposes one game port on the Service and container.
type GamePort struct {
	// Name of the port (lowercase, used in Service/ContainerPort names).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	Name string `json:"name"`

	// Port the game listens on. Used as containerPort, service port, and
	// (unless overridden) nodePort.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Protocol of the port.
	// +kubebuilder:validation:Enum=TCP;UDP
	Protocol corev1.Protocol `json:"protocol"`

	// NodePort optionally pins the Service nodePort (must be inside the
	// cluster's node-port range). Omit to let the API server allocate.
	// +optional
	NodePort *int32 `json:"nodePort,omitempty"`

	// HostPort additionally binds the port on the node. Belt-and-suspenders
	// for relay DNAT setups; avoid unless needed.
	// +optional
	HostPort *int32 `json:"hostPort,omitempty"`
}

// GameVolume is one persistent volume for game data (world, config, server
// binaries). One PVC is created per volume unless ExistingClaim is set.
type GameVolume struct {
	// Name of the volume/PVC.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// MountPath inside the game container.
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`

	// Size of the created PVC. Ignored when ExistingClaim is set.
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName of the created PVC. Nil uses the cluster default.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ExistingClaim mounts an existing PVC instead of creating one.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`

	// Backup includes this volume in backups. Defaults to true; set false for
	// cache volumes (e.g. server binaries re-downloadable via SteamCMD).
	// +optional
	// +kubebuilder:default=true
	Backup bool `json:"backup,omitempty"`
}

// LifecycleHooks are exec commands run inside the game container around
// lifecycle events. Commands are argv arrays (no shell).
type LifecycleHooks struct {
	// Container to exec into. Defaults to the first (game) container.
	// +optional
	Container string `json:"container,omitempty"`

	// PreSaveCommand triggers a world save before backups and graceful stops
	// (e.g. ["rcon-cli", "save-all"]).
	// +optional
	PreSaveCommand []string `json:"preSaveCommand,omitempty"`

	// PreSaveWaitSeconds to wait after PreSaveCommand before continuing.
	// +optional
	// +kubebuilder:default=10
	PreSaveWaitSeconds int32 `json:"preSaveWaitSeconds,omitempty"`

	// StopCommand gracefully stops the server (e.g. ["rcon-cli", "stop"]).
	// Used by scale-to-zero and pod deletion.
	// +optional
	StopCommand []string `json:"stopCommand,omitempty"`

	// StopWaitSeconds to wait after StopCommand before SIGTERM.
	// +optional
	// +kubebuilder:default=15
	StopWaitSeconds int32 `json:"stopWaitSeconds,omitempty"`
}

// QuerySpec configures the game query protocol for player counts and status.
type QuerySpec struct {
	// Type of query protocol.
	Type QueryType `json:"type"`

	// Port to query. Usually the game port (A2S derives the query port
	// automatically on many games) or the explicit query port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Host override. Defaults to the in-cluster service DNS name.
	// +optional
	Host string `json:"host,omitempty"`
}

// IdlePolicy scales the game to zero after a period with no players.
type IdlePolicy struct {
	// Enabled turns on idle scale-to-zero.
	Enabled bool `json:"enabled"`

	// Query protocol used to count players.
	Query QuerySpec `json:"query"`

	// TimeoutMinutes with zero players before scaling to 0.
	// +optional
	// +kubebuilder:default=30
	TimeoutMinutes int32 `json:"timeoutMinutes,omitempty"`

	// IntervalSeconds between player-count checks.
	// +optional
	// +kubebuilder:default=120
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
}

// WakeOnConnectPolicy fronts the game with a lightweight proxy that listens
// for incoming connections and starts the game on first connect (lazymc-style).
// Currently only meaningful for Minecraft-protocol games.
type WakeOnConnectPolicy struct {
	// Enabled turns on the wake-on-connect proxy.
	Enabled bool `json:"enabled"`

	// ProxyImage is the proxy container image.
	// +optional
	// +kubebuilder:default="ghcr.io/timvisee/lazymc:latest"
	ProxyImage string `json:"proxyImage,omitempty"`
}

// SteamWorkshopConfig downloads Steam Workshop items via an init container
// running steamcmd, and injects STEAM_WORKSHOP_ITEMS into the game container
// for images that self-manage workshop content.
type SteamWorkshopConfig struct {
	// AppID of the game (the dedicated-server app, not the workshop app, for
	// most titles; e.g. 896660 for Valheim, 380870 for Zomboid).
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appId"`

	// Items are Steam Workshop item IDs to download.
	// +optional
	Items []int64 `json:"items,omitempty"`

	// MountPath where items are downloaded. Must overlap a GameVolume to be
	// persistent. Defaults to the first volume's mountPath.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// ModConfig configures game mod/content surfaces.
type ModConfig struct {
	// SteamWorkshop configures Steam Workshop item downloads.
	// +optional
	SteamWorkshop *SteamWorkshopConfig `json:"steamWorkshop,omitempty"`
}

// RetentionPolicy controls how many restic snapshots are kept.
type RetentionPolicy struct {
	// +optional
	// +kubebuilder:default=3
	KeepLast int32 `json:"keepLast,omitempty"`
	// +optional
	// +kubebuilder:default=7
	KeepDaily int32 `json:"keepDaily,omitempty"`
	// +optional
	// +kubebuilder:default=4
	KeepWeekly int32 `json:"keepWeekly,omitempty"`
	// +optional
	// +kubebuilder:default=3
	KeepMonthly int32 `json:"keepMonthly,omitempty"`
}

// S3Destination points restic at an S3-compatible bucket (AWS, GCS via
// interoperability keys, B2, MinIO).
type S3Destination struct {
	// Endpoint of the S3 API (e.g. "s3.us-west-1.amazonaws.com",
	// "storage.googleapis.com"). Empty uses AWS defaults.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Prefix inside the bucket for this game's repository. Defaults to the
	// HostedGame name.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region for AWS/GCS. Optional for most S3-compatible stores.
	// +optional
	Region string `json:"region,omitempty"`

	// CredentialsSecretRef names a Secret in the HostedGame's namespace with
	// keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and RESTIC_PASSWORD.
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// BackupSpec configures scheduled backups. The operator renders a per-game
// CronJob that runs the game's PreSaveCommand, then uploads with the selected
// driver.
type BackupSpec struct {
	// Driver selects the backup implementation.
	// +kubebuilder:default=restic
	Driver BackupDriver `json:"driver"`

	// Schedule in cron format (cluster-local time).
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// Suspend pauses scheduled backups without deleting the CronJob.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Retention controls snapshot pruning.
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`

	// S3 destination (required for driver=restic).
	// +optional
	S3 *S3Destination `json:"s3,omitempty"`
}

// DiscordSpec controls how this game appears in the operator's Discord bot.
type DiscordSpec struct {
	// Enabled exposes this game to Discord commands. Games are hidden by
	// default so internal servers don't leak into Discord.
	Enabled bool `json:"enabled"`

	// Description shown in /game info and /game list.
	// +optional
	Description string `json:"description,omitempty"`

	// PublicHost players connect to (e.g. "valheim.example.com:2456").
	// +optional
	PublicHost string `json:"publicHost,omitempty"`

	// ConnectionHint with client setup instructions (markdown allowed).
	// +optional
	ConnectionHint string `json:"connectionHint,omitempty"`

	// PasswordSecretRef points at the join password to DM on request.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// Category groups this game in /game list (e.g. "minecraft", "survival").
	// +optional
	Category string `json:"category,omitempty"`
}

// HostedGameSpec defines one persistent, Discord-managed game server.
type HostedGameSpec struct {
	// DisplayName is the human-facing server name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Image of the game server container.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// ImagePullPolicy for the game container.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// Env for the game container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom for the game container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Command overrides the image entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args for the command.
	// +optional
	Args []string `json:"args,omitempty"`

	// Resources for the game container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector for the game pod.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity for the game pod.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations for the game pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Ports exposed by the game. At least one is required.
	// +kubebuilder:validation:MinItems=1
	Ports []GamePort `json:"ports"`

	// Volumes for persistent game data.
	// +optional
	Volumes []GameVolume `json:"volumes,omitempty"`

	// Lifecycle hooks for saves and graceful stops.
	// +optional
	Lifecycle *LifecycleHooks `json:"lifecycle,omitempty"`

	// StartupProbe for the game container. A process-running exec probe is
	// generated when omitted and Lifecycle is set; otherwise no probe.
	// +optional
	StartupProbe *corev1.Probe `json:"startupProbe,omitempty"`

	// LivenessProbe for the game container.
	// +optional
	LivenessProbe *corev1.Probe `json:"livenessProbe,omitempty"`

	// Idle scale-to-zero policy.
	// +optional
	Idle *IdlePolicy `json:"idle,omitempty"`

	// WakeOnConnect proxy (lazymc-style).
	// +optional
	WakeOnConnect *WakeOnConnectPolicy `json:"wakeOnConnect,omitempty"`

	// Mods content surfaces (Steam Workshop).
	// +optional
	Mods *ModConfig `json:"mods,omitempty"`

	// Backup schedule and destination. Omit to disable backups.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// Discord visibility and metadata.
	// +optional
	Discord *DiscordSpec `json:"discord,omitempty"`
}

// PlayerStatus is the last observed player count from the query protocol.
type PlayerStatus struct {
	Online int32    `json:"online"`
	Max    int32    `json:"max,omitempty"`
	Names  []string `json:"names,omitempty"`
	// ObservedAt is when this count was last refreshed.
	ObservedAt metav1.Time `json:"observedAt,omitempty"`
}

// BackupStatus records the most recent backup attempt.
type BackupStatus struct {
	// LastScheduleTime of the last attempt.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastResult is Succeeded or Failed.
	// +optional
	LastResult string `json:"lastResult,omitempty"`

	// Message with failure detail or snapshot id.
	// +optional
	Message string `json:"message,omitempty"`
}

// HostedGameStatus is controller-owned observed state.
type HostedGameStatus struct {
	// Phase of the server lifecycle.
	// +optional
	Phase GamePhase `json:"phase,omitempty"`

	// Address players use in-cluster or via the LoadBalancer VIP.
	// +optional
	Address string `json:"address,omitempty"`

	// Players last observed via the query protocol.
	// +optional
	Players *PlayerStatus `json:"players,omitempty"`

	// LastStartedAt is when the server last transitioned to Running.
	// +optional
	LastStartedAt *metav1.Time `json:"lastStartedAt,omitempty"`

	// Backup status summary.
	// +optional
	Backup *BackupStatus `json:"backup,omitempty"`

	// Conditions of the resource. Bounded set: Ready, BackupReady, Queried.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration of the last reconcile.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ControllerVersion that wrote this status.
	// +optional
	ControllerVersion string `json:"controllerVersion,omitempty"`
}

// HostedGame is one persistent game server managed by the operator:
// workload, storage, exposure, idle scaling, backups, mods, and Discord
// surface in a single resource.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hg
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.players.online`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HostedGame struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostedGameSpec   `json:"spec,omitempty"`
	Status HostedGameStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HostedGameList contains a list of HostedGame.
type HostedGameList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostedGame `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostedGame{}, &HostedGameList{})
}
