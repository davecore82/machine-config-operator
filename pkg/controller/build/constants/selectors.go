package constants

import (
	"strings"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	mcfgv1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

func MachineOSBuildSelector(mosc *mcfgv1alpha1.MachineOSConfig, mcp *mcfgv1.MachineConfigPool) labels.Selector {
	return labels.SelectorFromSet(map[string]string{
		TargetMachineConfigPoolLabelKey: mcp.Name,
		RenderedMachineConfigLabelKey:   mcp.Spec.Configuration.Name,
		MachineOSConfigNameLabelKey:     mosc.Name,
	})
}

// Returns a selector with the appropriate labels for an OS build object label
// query.
func OSBuildSelector() labels.Selector {
	return labelsToSelector([]string{
		OnClusterLayeringLabelKey,
		RenderedMachineConfigLabelKey,
		TargetMachineConfigPoolLabelKey,
	})
}

// Returns a selector with the appropriate labels for an ephemeral build object
// label query.
func EphemeralBuildObjectSelector() labels.Selector {
	return labelsToSelector([]string{
		EphemeralBuildObjectLabelKey,
		OnClusterLayeringLabelKey,
		RenderedMachineConfigLabelKey,
		TargetMachineConfigPoolLabelKey,
	})
}

func EphemeralBuildObjectSelectorForSpecificBuild(mosb *mcfgv1alpha1.MachineOSBuild, mosc *mcfgv1alpha1.MachineOSConfig) (labels.Selector, error) {
	selector := labelsToSelector([]string{
		EphemeralBuildObjectLabelKey,
		OnClusterLayeringLabelKey,
	})

	renderedMCSelector, err := labels.NewRequirement(RenderedMachineConfigLabelKey, selection.Equals, []string{mosb.Spec.DesiredConfig.Name})
	if err != nil {
		return nil, err
	}

	mcpSelector, err := labels.NewRequirement(TargetMachineConfigPoolLabelKey, selection.Equals, []string{mosc.Spec.MachineConfigPool.Name})
	if err != nil {
		return nil, err
	}

	return selector.Add(*renderedMCSelector, *mcpSelector), nil
}

// Returns a selector with the appropriate labels for a canonicalized secret
// label query.
func CanonicalizedSecretSelector() labels.Selector {
	return labelsToSelector([]string{
		CanonicalSecretLabelKey,
		OriginalSecretNameLabelKey,
		OnClusterLayeringLabelKey,
	})
}

// Takes a list of label keys and converts them into a Selector object that
// will require all label keys to be present.
func labelsToSelector(requiredLabels []string) labels.Selector {
	reqs := []labels.Requirement{}

	for _, label := range requiredLabels {
		req, err := labels.NewRequirement(label, selection.Exists, []string{})
		if err != nil {
			panic(err)
		}

		reqs = append(reqs, *req)
	}

	return labels.NewSelector().Add(reqs...)
}

// Determines if a given object was created by BuildController. This is mostly
// useful for tests and other helpers that may need to clean up after a failed
// run. It first determines if the object is an ephemeral build object, next it
// checks whether the object has all of the required labels, next it checks if
// the object is a canonicalized secret, and finally, it checks whether the
// object is a MachineOSBuild.
func IsObjectCreatedByBuildController(obj metav1.Object) bool {
	if isEphemeralBuildObject(obj) {
		return true
	}

	if hasAllRequiredOSBuildLabels(obj.GetLabels()) {
		return true
	}

	secret, ok := obj.(*corev1.Secret)
	if ok && isCanonicalizedSecret(secret) {
		return true
	}

	if _, ok := obj.(*mcfgv1alpha1.MachineOSBuild); ok {
		return true
	}

	return false
}

// Determines if a secret has been canonicalized by us by checking both for the
// suffix as well as the labels that we add to the canonicalized secret.
func isCanonicalizedSecret(secret *corev1.Secret) bool {
	return hasCanonicalizedSecretLabels(secret) && strings.HasSuffix(secret.Name, canonicalSecretSuffix)
}

// Determines if a secret has our canonicalized secret label.
func hasCanonicalizedSecretLabels(secret *corev1.Secret) bool {
	return CanonicalizedSecretSelector().Matches(labels.Set(secret.Labels))
}

// Determines if an object is an ephemeral build object by examining its labels.
func isEphemeralBuildObject(obj metav1.Object) bool {
	return EphemeralBuildObjectSelector().Matches(labels.Set(obj.GetLabels()))
}

// Determines if an object is managed by this controller by examining its labels.
func hasAllRequiredOSBuildLabels(inLabels map[string]string) bool {
	return OSBuildSelector().Matches(labels.Set(inLabels))
}
