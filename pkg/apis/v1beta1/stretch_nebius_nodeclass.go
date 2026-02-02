package v1beta1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=stretchnebiusnodeclasses,scope=Cluster,categories={karpenter,nap},shortName={stnbnc,stnbncs}
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type StretchNebiusNodeClass struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// status contains the resolved state of the StretchNebiusNodeClass.
	// +optional
	Status StretchNebiusNodeClassStatus `json:"status,omitempty"`
}

type StretchNebiusNodeClassStatus struct {
	// conditions contains signals for health and readiness
	// +optional
	//nolint:kubeapilinter // conditions: using status.Condition from operatorpkg instead of metav1.Condition for compatibility
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type StretchNebiusNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StretchNebiusNodeClass `json:"items"`
}
