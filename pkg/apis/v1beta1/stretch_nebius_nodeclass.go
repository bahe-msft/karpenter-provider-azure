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

	// +optional
	Spec StretchNebiusNodeClassSpec `json:"spec,omitempty"`

	// status contains the resolved state of the StretchNebiusNodeClass.
	// +optional
	Status StretchNebiusNodeClassStatus `json:"status,omitempty"`
}

var _ status.Object = (*StretchNebiusNodeClass)(nil)

func (s *StretchNebiusNodeClass) GetConditions() []status.Condition {
	return s.Status.Conditions
}

func (s *StretchNebiusNodeClass) SetConditions(conditions []status.Condition) {
	s.Status.Conditions = conditions
}

func (s *StretchNebiusNodeClass) StatusConditions() status.ConditionSet {
	conds := []string{
		ConditionTypeValidationSucceeded,
	}

	return status.NewReadyConditions(conds...).For(s)
}

type StretchNebiusNodeClassSpec struct {
	// SubnetID is the nebius subnet id to launch nodes in.
	// Node will be auto-assigned an IP from this subnet.
	// +required
	SubnetID string `json:"subnetID,omitempty"`
	// OSDiskSizeGB is the size of the OS disk in GB.
	// +default=128
	// +optional
	OSDiskSizeGB *int32 `json:"osDiskSizeGB,omitempty"`
	// +default="ubuntu24.04-driverless"
	// +optional
	OSDiskImageFamily *string `json:"osDiskImageFamily,omitempty"`

	// +default=false
	// +optional
	AllocateNodePublicIP *bool `json:"allocateNodePublicIP,omitempty"`

	// TODO: other fields (kublet etc)
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
