package nebius

import (
	"strings"

	nebiuscomputev1 "github.com/nebius/gosdk/proto/nebius/compute/v1"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	labelspkg "github.com/Azure/karpenter-provider-azure/pkg/providers/labels"
)

func nodeClaimFromInstance(
	instance *nebiuscomputev1.Instance,
	instanceType *cloudprovider.InstanceType,
) *v1.NodeClaim {
	rv := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              strings.TrimPrefix(instance.Metadata.GetName(), instanceNamePrefix),
			Labels:            map[string]string{},
			Annotations:       map[string]string{},
			CreationTimestamp: metav1.NewTime(instance.Metadata.GetCreatedAt().AsTime()),
		},
		Spec: v1.NodeClaimSpec{},
		Status: v1.NodeClaimStatus{
			ProviderID: vmInstanceProviderID(instance.Metadata.GetId()),
		},
	}

	if instance.Status.State == nebiuscomputev1.InstanceStatus_DELETING {
		rv.DeletionTimestamp = lo.ToPtr(metav1.Now())
	}

	rv.Labels = labelspkg.GetAllSingleValuedRequirementLabels(instanceType.Requirements)
	rv.Status.Capacity = lo.PickBy(instanceType.Capacity, filterNoneZeroResource)
	rv.Status.Allocatable = lo.PickBy(instanceType.Allocatable(), filterNoneZeroResource)

	// TODO: zone from instance
	// TODO: labels from instance

	return rv
}
