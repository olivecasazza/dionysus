package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/workshop"
)

// label keys shared across every object the operator creates.
const (
	labelName     = "app.kubernetes.io/name"
	labelInstance = "app.kubernetes.io/instance"
	labelManaged  = "app.kubernetes.io/managed-by"
	labelComp     = "app.kubernetes.io/component"
)

// commonLabels returns the labels applied to every object the operator
// owns for a given HostedGame. Caller may merge more keys on top.
func commonLabels(game *gamesv1alpha1.HostedGame) map[string]string {
	return map[string]string{
		labelName:     "hostedgame",
		labelInstance: game.Name,
		labelManaged:  "dionysus",
		labelComp:     "game",
	}
}

// selectorLabels is the strict subset used as the Deployment selector and
// pod template labels. Never changes after creation (immutable).
func selectorLabels(game *gamesv1alpha1.HostedGame) map[string]string {
	return map[string]string{
		labelName:     "hostedgame",
		labelInstance: game.Name,
		labelManaged:  "dionysus",
	}
}

// GameDeployment renders the desired Deployment for a HostedGame. The
// controller diffs this against the cluster and reconciles.
func GameDeployment(game *gamesv1alpha1.HostedGame) (*appsv1.Deployment, error) {
	pullPolicy := game.Spec.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	// Build container ports from spec.ports.
	ports := make([]corev1.ContainerPort, 0, len(game.Spec.Ports))
	for _, p := range game.Spec.Ports {
		cp := corev1.ContainerPort{
			Name:          p.Name,
			ContainerPort: p.Port,
			Protocol:      p.Protocol,
		}
		if p.HostPort != nil {
			cp.HostPort = *p.HostPort
		}
		ports = append(ports, cp)
	}

	// Build volumeMounts from spec.volumes.
	mounts := make([]corev1.VolumeMount, 0, len(game.Spec.Volumes))
	for _, v := range game.Spec.Volumes {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      v.Name,
			MountPath: v.MountPath,
		})
	}

	// Build pod volumes. Each spec.volume maps to a PVC reference: either
	// the user-provided existingClaim, or the operator-managed
	// "<game.Name>-<volume.Name>".
	vols := make([]corev1.Volume, 0, len(game.Spec.Volumes))
	for _, v := range game.Spec.Volumes {
		claim := v.ExistingClaim
		if claim == "" {
			claim = game.Name + "-" + v.Name
		}
		vols = append(vols, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim,
				},
			},
		})
	}

	// Game container env: spec.env + workshop env injection (if any).
	env := game.Spec.Env
	if wenv := workshop.Env(game); wenv != nil {
		env = append(env, wenv...)
	}

	gameContainer := corev1.Container{
		Name:            "game",
		Image:           game.Spec.Image,
		ImagePullPolicy: pullPolicy,
		Env:             env,
		EnvFrom:         game.Spec.EnvFrom,
		Command:         game.Spec.Command,
		Args:            game.Spec.Args,
		Resources:       game.Spec.Resources,
		Ports:           ports,
		VolumeMounts:    mounts,
		StartupProbe:    game.Spec.StartupProbe,
		LivenessProbe:   game.Spec.LivenessProbe,
	}

	// Probes: if lifecycle is configured but no startup probe was provided,
	// the operator *could* synthesize an exec probe against the stop hook.
	// TODO(controller): synthesize exec probe when lifecycle != nil and
	// startupProbe == nil. For now leave nil; games that need it must
	// provide their own probe.

	containers := []corev1.Container{gameContainer}

	// Wake-on-connect: a lightweight front proxy (lazymc-style) that listens
	// for incoming connections and starts the game on first connect.
	// The proxy's config injection (routing target, world-save hooks) is a
	// future lane; for now we just place the sidecar.
	if game.Spec.WakeOnConnect != nil && game.Spec.WakeOnConnect.Enabled {
		// TODO(wake): inject lazymc config via a ConfigMap volume mounted into
		// the proxy, pointing at the game container's primary port. Currently
		// unconfigured — the sidecar starts but won't forward.
		proxyPorts := []corev1.ContainerPort{}
		if len(ports) > 0 {
			proxyPorts = append(proxyPorts, corev1.ContainerPort{
				Name:          ports[0].Name + "-proxy",
				ContainerPort: ports[0].ContainerPort,
				Protocol:      ports[0].Protocol,
			})
		}
		containers = append(containers, corev1.Container{
			Name:            "wake-proxy",
			Image:           game.Spec.WakeOnConnect.ProxyImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Ports:           proxyPorts,
		})
	}

	podSpec := corev1.PodSpec{
		Containers:   containers,
		NodeSelector: game.Spec.NodeSelector,
		Affinity:     game.Spec.Affinity,
		Tolerations:  game.Spec.Tolerations,
		Volumes:      vols,
	}

	// Steam Workshop init container (nil when not configured).
	if ic := workshop.InitContainer(game); ic != nil {
		podSpec.InitContainers = append(podSpec.InitContainers, *ic)
	}

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      game.Name,
			Namespace: game.Namespace,
			Labels:    commonLabels(game),
		},
		Spec: appsv1.DeploymentSpec{
			// Replica count is always 1 here; idle scale-to-zero is driven
			// by the idle lane, which patches replicas separately rather
			// than owning the field here.
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels(game),
			},
			Strategy: appsv1.DeploymentStrategy{
				// Games are stateful; the operator deliberately treats them
				// as single-replica and does not roll new alongside old.
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: selectorLabels(game),
				},
				Spec: podSpec,
			},
		},
	}

	if err := SetOwnerReference(game, dep); err != nil {
		return nil, err
	}
	return dep, nil
}

// GameService renders the Service fronting the game pods. Always ClusterIP;
// LB / NodePort / relay DNAT is handled outside the operator (e.g. Cilium
// L2 LB or the hetzner-relay iptables setup).
func GameService(game *gamesv1alpha1.HostedGame) (*corev1.Service, error) {
	ports := make([]corev1.ServicePort, 0, len(game.Spec.Ports))
	for _, p := range game.Spec.Ports {
		sp := corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: intstr.FromString(p.Name),
			Protocol:   p.Protocol,
		}
		if p.NodePort != nil {
			sp.NodePort = *p.NodePort
		}
		ports = append(ports, sp)
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      game.Name,
			Namespace: game.Namespace,
			Labels:    commonLabels(game),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(game),
			Ports:    ports,
		},
	}

	if err := SetOwnerReference(game, svc); err != nil {
		return nil, err
	}
	return svc, nil
}

// GamePVCs renders the operator-managed PVCs for a game. One PVC per
// spec.volume without an existingClaim. Volumes that reference an
// existingClaim are skipped (mounted directly via the Pod volume).
func GamePVCs(game *gamesv1alpha1.HostedGame) ([]*corev1.PersistentVolumeClaim, error) {
	pvcs := make([]*corev1.PersistentVolumeClaim, 0, len(game.Spec.Volumes))
	for _, v := range game.Spec.Volumes {
		if v.ExistingClaim != "" {
			continue
		}

		spec := corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		}
		if !v.Size.IsZero() {
			spec.Resources.Requests = corev1.ResourceList{
				corev1.ResourceStorage: v.Size,
			}
		}
		if v.StorageClassName != nil {
			spec.StorageClassName = v.StorageClassName
		}

		pvc := &corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      game.Name + "-" + v.Name,
				Namespace: game.Namespace,
				Labels:    commonLabels(game),
			},
			Spec: spec,
		}

		if err := SetOwnerReference(game, pvc); err != nil {
			return nil, err
		}
		pvcs = append(pvcs, pvc)
	}
	return pvcs, nil
}

func int32Ptr(i int32) *int32 { return &i }
