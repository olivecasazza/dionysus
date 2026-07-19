// Package scheme holds the shared runtime.Scheme for the operator. The
// controller manager, the resource renderers, the backup driver, and any
// other component that needs to set owner references or serialize objects
// import this package instead of building their own scheme.
package scheme

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// Scheme is the single shared scheme used by every operator component.
// Adding a type here makes it available for ownership, serialization, and
// caching everywhere.
var Scheme = runtime.NewScheme()

// Codecs is the codec factory built from Scheme. Useful for decoding
// embedded YAML samples or building REST clients.
var Codecs = serializer.NewCodecFactory(Scheme)

func init() {
	// client-go's metaschema adds core/v1 and the metav1 internals.
	_ = clientgoscheme.AddToScheme(Scheme)
	_ = appsv1.AddToScheme(Scheme)
	_ = batchv1.AddToScheme(Scheme)
	_ = corev1.AddToScheme(Scheme)
	_ = gamesv1alpha1.AddToScheme(Scheme)
}
