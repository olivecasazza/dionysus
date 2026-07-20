// Package controller implements the HostedGame reconcile loop. It owns
// the Deployment, Service, and PVCs for every HostedGame and surfaces
// observed state into HostedGameStatus.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/idle"
	"github.com/olivecasazza/dionysus/internal/resources"
)

const (
	// runningRequeue is how often a Running game is requeued so the idle /
	// query lane can refresh player counts. The controller itself doesn't
	// need to reconcile this often, but the periodic requeue hands control
	// back to idle-scaled games cheaply.
	runningRequeue = 30 * time.Second
)

// HostedGameReconciler owns the workload children of a HostedGame.
type HostedGameReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=games.dionysus.io,resources=hostedgames,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=games.dionysus.io,resources=hostedgames/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=games.dionysus.io,resources=hostedgames/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="batch",resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="batch",resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a HostedGame toward its desired state: PVCs created,
// Deployment and Service present and matching spec, status observed.
func (r *HostedGameReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the HostedGame. NotFound is a clean delete — ownerRef GC
	//    handles the children.
	game := &gamesv1alpha1.HostedGame{}
	if err := r.Get(ctx, req.NamespacedName, game); err != nil {
		if apierrors.IsNotFound(err) {
			// Clear the deleted game's metric series so dashboards don't
			// retain a stale game.
			deleteGameMetrics(req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Backup wiring is the backup lane's responsibility. Surface it as
	//    a log line so it's visible in operator logs, but don't fail
	//    reconcile: the game still runs without backups.
	if game.Spec.Backup != nil {
		logger.Info("backup configured but not yet wired; spec.backup ignored",
			"driver", game.Spec.Backup.Driver,
			"schedule", game.Spec.Backup.Schedule)
	}

	// 3. PVCs. Created if absent; not updated on size change (PVC resize is
	//    the user's job, and most storage drivers reject shrink anyway).
	if err := r.reconcilePVCs(ctx, game); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile PVCs: %w", err)
	}

	// 4. Deployment. CreateOrUpdate diff against the rendered desired spec.
	dep, err := r.reconcileDeployment(ctx, game)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Deployment: %w", err)
	}

	// 5. Service.
	if _, err := r.reconcileService(ctx, game); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile Service: %w", err)
	}

	// 6. Idle policy evaluation. Queries the game server, writes player
	//    status, and recommends scale-to-zero when applicable. Run
	//    before computeStatus so the status patch picks up Players.
	idleDecision, err := idle.Evaluate(ctx, game, dep)
	if err != nil {
		// Log + continue. A transient query failure shouldn't bounce
		// the workqueue; the periodic requeue re-tries.
		logger.Error(err, "idle evaluation failed")
	} else {
		if err := r.applyIdleDecision(ctx, game, idleDecision); err != nil {
			logger.Error(err, "apply idle decision failed")
		}
		// Re-read dep if we just scaled it so computeStatus sees reality.
		if idleDecision.ScaleToZero {
			if err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, dep); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-read dep after scale: %w", err)
			}
		}
	}

	// 7. Status. Compute desired phase, address, observedGeneration, and
	//    patch if changed. Players was set above by the idle evaluation.
	newStatus := computeStatus(game, dep)
	if idleDecision.Players != nil {
		newStatus.Players = idleDecision.Players
	}
	// Publish observed state to Prometheus every reconcile (cheap gauge sets),
	// so phase/players/backup metrics track reality without waiting on a status
	// change.
	recordGameMetrics(game, newStatus)
	if !statusEqual(game.Status, newStatus) {
		patchBase := client.MergeFrom(game.DeepCopy())
		game.Status = newStatus
		if err := r.Status().Patch(ctx, game, patchBase); err != nil {
			// Status patch failures are recoverable — don't bounce the
			// workqueue indefinitely.
			logger.Error(err, "failed to patch status")
			return ctrl.Result{RequeueAfter: runningRequeue}, nil
		}
	}

	// 8. Requeue cadence. Idle-aware games use their configured interval;
	//    everything else uses the controller default. Stopped/Failed
	//    games only requeue on spec change.
	requeue := requeueFor(newStatus.Phase, idleDecision.RequeueAfter)
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

// reconcilePVCs creates any missing PVCs. Existing PVCs are not resized;
// PVC spec.resources.requests[storage] is immutable after creation in most
// storage drivers, so the operator treats it as create-only.
func (r *HostedGameReconciler) reconcilePVCs(ctx context.Context, game *gamesv1alpha1.HostedGame) error {
	desired, err := resources.GamePVCs(game)
	if err != nil {
		return err
	}
	for _, pvc := range desired {
		existing := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, existing)
		if err == nil {
			// Exists — leave it alone. Size changes require user action.
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// reconcileDeployment uses CreateOrUpdate: the rendered Deployment is the
// desired state, and controllerutil reconciles labels, annotations, and
// the spec.template subset. Returns the live Deployment for status
// computation.
func (r *HostedGameReconciler) reconcileDeployment(ctx context.Context, game *gamesv1alpha1.HostedGame) (*appsv1.Deployment, error) {
	desired, err := resources.GameDeployment(game)
	if err != nil {
		return nil, err
	}

	live := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, live)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Re-fetch so callers see the just-created object's managed fields.
		if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, live); err != nil {
			return nil, err
		}
		return live, nil
	}
	if err != nil {
		return nil, err
	}

	// Mutate live in place toward desired. We update the mutable parts:
	// labels, annotations, replicas, and the pod template. selector is
	// immutable and never changes anyway (it's derived from constant
	// selector labels).
	updated := live.DeepCopy()
	updated.Labels = desired.Labels
	if desired.Spec.Replicas != nil {
		updated.Spec.Replicas = desired.Spec.Replicas
	}
	// Idle lane can scale the Deployment to 0. If the live Deployment's
	// replicas are 0 (idle-scaled), preserve that decision and don't fight
	// it. We only bump back to 1 when the user explicitly wakes the game.
	if live.Spec.Replicas != nil && *live.Spec.Replicas == 0 {
		updated.Spec.Replicas = int32Ptr(0)
	}
	updated.Spec.Template = desired.Spec.Template
	updated.Spec.Strategy = desired.Spec.Strategy

	if !reflect.DeepEqual(live.Labels, updated.Labels) ||
		!reflect.DeepEqual(live.Spec.Replicas, updated.Spec.Replicas) ||
		!reflect.DeepEqual(live.Spec.Template, updated.Spec.Template) ||
		!reflect.DeepEqual(live.Spec.Strategy, updated.Spec.Strategy) {
		if err := r.Update(ctx, updated); err != nil {
			return nil, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, live); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// reconcileService creates or updates the game Service.
func (r *HostedGameReconciler) reconcileService(ctx context.Context, game *gamesv1alpha1.HostedGame) (*corev1.Service, error) {
	desired, err := resources.GameService(game)
	if err != nil {
		return nil, err
	}

	live := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, live)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}

	// Service spec.clusterIP and allocated ports are immutable. We only
	// sync labels, ports we added, and selector.
	updated := live.DeepCopy()
	updated.Labels = desired.Labels
	updated.Spec.Selector = desired.Spec.Selector
	updated.Spec.Ports = desired.Spec.Ports

	if !reflect.DeepEqual(live.Labels, updated.Labels) ||
		!reflect.DeepEqual(live.Spec.Selector, updated.Spec.Selector) ||
		!reflect.DeepEqual(live.Spec.Ports, updated.Spec.Ports) {
		if err := r.Update(ctx, updated); err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// computeStatus derives the desired HostedGameStatus from the live
// Deployment. Heuristic: games are stateful and single-replica, so
// availableReplicas >= 1 means Running; replicas==0 means Stopped; any
// pods pending or the deployment not yet available means Starting.
func computeStatus(game *gamesv1alpha1.HostedGame, dep *appsv1.Deployment) gamesv1alpha1.HostedGameStatus {
	status := gamesv1alpha1.HostedGameStatus{
		ControllerVersion: "0.1.0",
	}

	if dep == nil {
		status.Phase = gamesv1alpha1.PhaseFailed
		status.ObservedGeneration = game.Generation
		return status
	}

	// Address: in-cluster DNS, only meaningful once the Service exists.
	// (Service is reconciled before computeStatus is called.)
	status.Address = fmt.Sprintf("%s.%s.svc.cluster.local", game.Name, game.Namespace)

	// Replicas == 0 means idle lane has scaled us down.
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
		status.Phase = gamesv1alpha1.PhaseStopped
		status.ObservedGeneration = game.Generation
		return status
	}

	// Available replica ⇒ Running.
	if dep.Status.AvailableReplicas >= 1 {
		status.Phase = gamesv1alpha1.PhaseRunning
	} else if dep.Status.UnavailableReplicas >= 1 {
		// Progressing — pods starting, image pulling, probe failing.
		status.Phase = gamesv1alpha1.PhaseStarting
	} else {
		// No available replicas, no unavailable replicas reported yet.
		// Newly-created Deployment, or status not yet observed.
		status.Phase = gamesv1alpha1.PhaseStarting
	}

	status.ObservedGeneration = game.Generation
	return status
}

// statusEqual reports whether two HostedGameStatus values are equivalent
// for the purposes of avoiding spurious status patches. Players and
// Backup fields are owned by other lanes — we don't touch them here — so
// we only compare the fields this controller sets.
func statusEqual(a, b gamesv1alpha1.HostedGameStatus) bool {
	if a.Phase != b.Phase {
		return false
	}
	if a.Address != b.Address {
		return false
	}
	if a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if a.ControllerVersion != b.ControllerVersion {
		return false
	}
	return true
}

// SetupWithManager registers the reconciler with the manager and watches
// owned children for enqueue-on-change.
func (r *HostedGameReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gamesv1alpha1.HostedGame{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

// applyIdleDecision persists the side effects of an idle.Decision:
//   - writes the LastNonZeroAnnotation when the idle lane wants to
//     record/reset the timer
//   - scales the Deployment to 0 when Decision.ScaleToZero is set
//
// Player status is folded into the newStatus computation in Reconcile,
// not here — this function only handles mutable bookkeeping + the
// scale-down action.
func (r *HostedGameReconciler) applyIdleDecision(
	ctx context.Context,
	game *gamesv1alpha1.HostedGame,
	decision idle.Decision,
) error {
	// Annotation update. We patch metadata only; status is patched
	// separately in Reconcile to avoid two status writes per loop.
	if decision.LastNonZeroAt != nil {
		annotations := game.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		newVal := decision.LastNonZeroAt.UTC().Format(time.RFC3339)
		if annotations[idle.LastNonZeroAnnotation] == newVal {
			// No-op; skip the patch.
		} else {
			annotations[idle.LastNonZeroAnnotation] = newVal
			patchBase := client.MergeFrom(game.DeepCopy())
			game.Annotations = annotations
			if err := r.Patch(ctx, game, patchBase); err != nil {
				return fmt.Errorf("patch idle annotation: %w", err)
			}
		}
	}

	// Scale to zero. We update the Deployment's replicas field directly.
	// The reconcileDeployment path already preserves replicas==0 once
	// set, so we only need to write the transition.
	if decision.ScaleToZero {
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: game.Name, Namespace: game.Namespace}, dep); err != nil {
			return fmt.Errorf("get deployment for scale-down: %w", err)
		}
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			return nil // already scaled
		}
		updated := dep.DeepCopy()
		zero := int32(0)
		updated.Spec.Replicas = &zero
		if err := r.Update(ctx, updated); err != nil {
			return fmt.Errorf("scale deployment to zero: %w", err)
		}
	}
	return nil
}

// requeueFor returns the requeue interval for a phase. Idle-aware games
// pass their configured interval (from idle.Decision.RequeueAfter);
// other phases use the controller default. Zero means "don't requeue"
// (Stopped / Failed wait on spec change).
func requeueFor(phase gamesv1alpha1.GamePhase, idleInterval time.Duration) time.Duration {
	if idleInterval > 0 {
		return idleInterval
	}
	switch phase {
	case gamesv1alpha1.PhaseRunning, gamesv1alpha1.PhaseStarting, gamesv1alpha1.PhaseStopping:
		return runningRequeue
	default:
		return 0
	}
}

// int32Ptr is duplicated from resources to avoid a cross-package import
// for a single helper.
func int32Ptr(i int32) *int32 { return &i }

// Ensure controllerutil is used (for SetControllerReference via resources);
// the import is load-bearing for the resources package's owner refs.
var _ = controllerutil.SetControllerReference
