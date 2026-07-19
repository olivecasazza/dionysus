package resources

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gamesv1alpha1 "github.com/olivecasazza/game-operator/api/v1alpha1"
	"github.com/olivecasazza/game-operator/internal/scheme"
)

// SetOwnerReference marks controlled as owned by the HostedGame owner.
// The controller-runtime GC uses this to cascade-delete children and to
// reconcile them when the owner changes.
func SetOwnerReference(owner *gamesv1alpha1.HostedGame, controlled metav1.Object) error {
	return controllerutil.SetControllerReference(owner, controlled, scheme.Scheme)
}
